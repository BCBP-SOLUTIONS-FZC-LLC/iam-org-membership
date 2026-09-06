#!/usr/bin/env python3
"""
run_need_test.py — Need_test sheet automation.

Each test GROUP gets its own freshly-provisioned tenant so tests never
contaminate each other. No external mock servers required.

Prerequisites:
    make docker-up   (Postgres + PgBouncer + Valkey)
    make mock-servers (Catalog Admin :8084 + RP :8083 + Workflow :8082)
    make run         (service on :8080)
"""
import json, re, time, uuid
import urllib.request, urllib.error
import openpyxl
from openpyxl.styles import PatternFill, Font, Alignment
from datetime import date

BASE       = "http://localhost:8080"
EXCEL_PATH = "/Users/sharmila/bcbp-solutions/XpertPMS/Org-Membership/Testing/o&g_testing.xlsx"
SHEET_NAME = "Need_test"

SYS_USER = "00000000-0000-0000-0000-000000000001"
SYS_TEN  = "aaaa0001-0001-0001-0001-000000000001"
DEPT_ENG = "de010001-0000-0000-0000-000000000001"
DEPT_DES = "de010002-0000-0000-0000-000000000002"
DEPT_OPS     = "de010006-0000-0000-0000-000000000006"  # non-system, is_active=true
DEPT_RETIRED = "de010007-0000-0000-0000-000000000007"  # non-system, is_active=false (retired)
DEPT_LOG     = "de010008-0000-0000-0000-000000000008"  # non-system, is_active=true
DEPT_ACC     = "de010009-0000-0000-0000-000000000009"  # non-system, is_active=true
NIL_UUID = "00000000-0000-0000-0000-000000000000"

def H(user, tenant, roles):
    return {"Content-Type":"application/json",
            "x-user-id":user,"x-tenant-id":tenant,"x-tenant-roles":roles}

H_SYS  = H(SYS_USER, SYS_TEN, "iam-system")
H_NOAUTH = {"Content-Type":"application/json"}
H_NOCT   = {"x-user-id":SYS_USER,"x-tenant-id":SYS_TEN,"x-tenant-roles":"iam-system"}

_tenant_counter = 9000  # high value to avoid collision with manual test tenants

def fresh_id():   return str(uuid.uuid4())
def fresh_tid():
    """Non-existent random tenant UUID — use for 404 tests."""
    return str(uuid.uuid4())

def group_tid(n):
    """Stable per-group tenant UUID (reused across runs, but managed by provision())."""
    return f"eeee{n:04d}-0001-0001-0001-000000000001"

# ── HTTP ──────────────────────────────────────────────────────────────────
def call(method, url, body=None, hdrs=None):
    if hdrs is None: hdrs = H_SYS
    data = json.dumps(body).encode() if body is not None else None
    req  = urllib.request.Request(url, data=data, headers=hdrs, method=method)
    try:
        r = urllib.request.urlopen(req, timeout=8)
        return r.status, json.loads(r.read() or b'{}')
    except urllib.error.HTTPError as e:
        try:    return e.code, json.loads(e.read())
        except: return e.code, {}
    except Exception as ex:
        return 0, {"_error": str(ex)}

def get_rv(url, hdrs=None):
    _, b = call("GET", url, None, hdrs)
    return b.get("record_version", 1)

# ── Per-group tenant state ────────────────────────────────────────────────
_group_counter = 0

class TenantState:
    """Holds a freshly provisioned tenant with owner/admin/member/suspended."""
    def __init__(self):
        global _group_counter
        _group_counter += 1
        self.tid       = group_tid(_group_counter)
        self.owner     = fresh_id()
        self.admin     = fresh_id()
        self.member    = fresh_id()
        self.suspended = fresh_id()
        self.invite_id = None
        self._provisioned = False

    def provision(self):
        if self._provisioned:
            return
        h = H(SYS_USER, self.tid, "iam-system")
        hop = H(SYS_USER, self.tid, "platform_operator")

        # Provision tenant (idempotent — returns 200 if already exists)
        import hashlib
        slug = f"g{hashlib.md5(self.tid.encode()).hexdigest()[:10]}"
        c_prov, b_prov = call("POST", f"{BASE}/api/v1/internal/tenants",
             {"tenant_id":self.tid,"slug":slug,"name":"Group Tenant",
              "plan":"starter","licensed_seats":30,"locale":"en-US",
              "owner_user_id":self.owner,"owner_email":"own@t.com","owner_name":"Own"}, h)
        if c_prov not in (200, 201):
            print(f"  ⚠ PROVISION FAILED {self.tid}: {c_prov} {b_prov}")

        # Always ensure all users are members (robust on second run)
        for uid, email, name in [
            (self.owner,    "own@t.com", "Owner"),
            (self.admin,    "adm@t.com", "Admin"),
            (self.member,   "mem@t.com", "Member"),
            (self.suspended,"sus@t.com", "Suspended"),
        ]:
            call("POST", f"{BASE}/api/v1/internal/tenants/{self.tid}/members",
                 {"user_id":uid,"email":email,"full_name":name}, h)

        # Ensure OWNER is active
        _, b = call("GET", f"{BASE}/api/v1/internal/tenants/{self.tid}/members/{self.owner}/exists",
                    None, h)
        rv_o = self._member_rv(self.owner)
        call("PATCH", f"{BASE}/api/v1/internal/tenants/{self.tid}/members/{self.owner}",
             {"status":"active","record_version":rv_o}, h)

        # Reassign ownership to s.owner (handles case where tenant existed from prev run)
        for _ in range(3):
            rv_t = self.tenant_rv()
            c2, _ = call("POST", f"{BASE}/api/v1/operator/tenants/{self.tid}/reassign-owner",
                         {"user_id":self.owner,"record_version":rv_t}, hop)
            if c2 == 200: break

        # Grant tenant_admin to admin (using iam-system for robustness)
        rv_a = self._member_rv(self.admin)
        call("PATCH", f"{BASE}/api/v1/internal/tenants/{self.tid}/members/{self.admin}",
             {"status":"active","record_version":rv_a}, h)
        # Use operator to grant tenant_admin (bypasses any role gate)
        call("PUT", f"{BASE}/api/v1/tenants/{self.tid}/members/{self.admin}/roles",
             {"roles":["tenant_admin"],"record_version":self._member_rv(self.admin)},
             H(self.owner, self.tid, "tenant_owner"))

        # Ensure MEMBER is active
        rv_m = self._member_rv(self.member)
        call("PATCH", f"{BASE}/api/v1/internal/tenants/{self.tid}/members/{self.member}",
             {"status":"active","record_version":rv_m}, h)

        # Suspend the suspended user
        rv_s = self._member_rv(self.suspended)
        call("PATCH", f"{BASE}/api/v1/internal/tenants/{self.tid}/members/{self.suspended}",
             {"status":"suspended","record_version":rv_s}, h)

        self._provisioned = True

    def _member_rv(self, uid):
        _, b = call("GET", f"{BASE}/api/v1/tenants/{self.tid}/members/{uid}", None,
                    H(SYS_USER, self.tid, "iam-system"))
        return b.get("record_version", 1)

    def tenant_rv(self):
        return get_rv(f"{BASE}/api/v1/tenants/{self.tid}",
                      H(SYS_USER, self.tid, "iam-system"))

    def member_rv(self, uid=None):
        return self._member_rv(uid or self.member)

    def Howner(self):  return H(self.owner,     self.tid, "tenant_owner")
    def Hadmin(self):  return H(self.admin,      self.tid, "tenant_admin")
    def Hmember(self): return H(self.member,     self.tid, "member")
    def Hop(self):     return H(SYS_USER,        self.tid, "platform_operator")
    def Hsys(self):    return H(SYS_USER,        self.tid, "iam-system")
    def Hcross(self):  return H(self.owner,      fresh_tid(), "tenant_owner")
    def Hnoauth(self): return H_NOAUTH

    def add_member(self, uid=None, email=None, name=None):
        uid = uid or fresh_id()
        email = email or f"{uid[:6]}@t.com"
        name  = name  or "User"
        call("POST", f"{BASE}/api/v1/internal/tenants/{self.tid}/members",
             {"user_id":uid,"email":email,"full_name":name}, self.Hsys())
        return uid

    def assign_dept(self, uid, dept=None, level="preparator"):
        dept = dept or DEPT_ENG
        rv   = 1
        # Try to get existing rv
        _, lb = call("GET", f"{BASE}/api/v1/tenants/{self.tid}/departments/{dept}/members",
                     None, self.Howner())
        for item in lb.get("items", []):
            if item.get("user_id") == uid:
                rv = item.get("record_version", 1)
                break
        call("PUT", f"{BASE}/api/v1/tenants/{self.tid}/departments/{dept}/members/{uid}",
             {"level":level,"record_version":rv}, self.Howner())

    def activate_dept(self, dept=DEPT_DES):
        call("POST", f"{BASE}/api/v1/tenants/{self.tid}/departments",
             {"department_id":dept}, self.Howner())

    def make_only_owner(self):
        """Strip admin of tenant_owner role so OWNER is the sole owner."""
        rv_a = self.member_rv(self.admin)
        call("PUT", f"{BASE}/api/v1/tenants/{self.tid}/members/{self.admin}/roles",
             {"roles":["tenant_admin"],"record_version":rv_a}, self.Howner())


# One state object per group (lazy-provisioned)
_states = {}
def get_state(group_key):
    if group_key not in _states:
        s = TenantState()
        s.provision()
        _states[group_key] = s
    return _states[group_key]

def state_for(tc):
    """Map a TC ID to a group key."""
    up = tc.upper()
    for prefix in ("I13","I11","IUM","I8","I5","I4","I3","I2","I15","I14","I9","I1"):
        if up.startswith(prefix): return get_state(prefix)
    for prefix in ("P28RL","P28","P27","P26","P25","P24","P2P19","P2","P12","P11",
                   "P10","P9","P8","P7","P6","P5","P4","P31","P30","P3","P13"):
        if up.startswith(prefix): return get_state(prefix)
    for prefix in ("O7","O4","GT","CROSS","CRS"):
        if up.startswith(prefix): return get_state(prefix)
    return get_state("misc")

# ── Skip logic ────────────────────────────────────────────────────────────
SKIP_PREFIXES  = ("I1-","P6-","P8-","P26-")
SKIP_TC_PATS   = ("-EVT-","-SQS-","-NOEVT-","-CONC-","-RACE-","-CON-",
                  "-DEP-","-TX-","-TM13-","-TM8-RACE-","-WFI-","-AUTH8-",
                  "-CONCURRENT-")
SKIP_SC_PATS   = ("concurrent","race condition","event emitted","enqueued",
                  "outbox","relay","downstream dep","db unavailable",
                  "connection pool","transaction","rollback","workflow service")

def skip_reason(tc, scenario):
    up = tc.upper()
    for p in SKIP_PREFIXES:
        if up.startswith(p.upper()): return f"needs {p.rstrip('-')} service"
    for p in SKIP_TC_PATS:
        if p.upper() in up: return p.strip("-")
    sc = scenario.lower()
    for p in SKIP_SC_PATS:
        if p in sc: return p[:20]
    return ""

# ── Complete TARGET list ──────────────────────────────────────────────────
TARGET = set("""
I1-H-01 I1-EVT-01 I1-EVT-03 I1-EVT-02 I1-EVT-04
I3-EVT-01 I3-EVT-02 I3-EVT-03 I4-EVT-01 I5-EVT-01
I5-HP-02 I5-HP-03 I5-EDGE-02 I5-BL-04
I13-HP-02 I13-EDGE-01 I13-AUTH-02 I13-BL-04 I13-BL-01 I13-BL-03
I13-HP-03 I13-SEC-01 I13-EVT-01 I13-HP-01 I13-DEP-01
O4-NOEVT-01 O7-H-03 O7-H-05 O7-BL-09 O7-BL-11
O7-EVT-01 O7-EVT-02 O7-EVT-04 O7-NOEVT-01
P2-EVT-01 P10-HAPPY-01 P10-SQS-01 P10-SQS-02 P10-HAPPY-02 P10-HAPPY-03
P6-EVT-01 P28-EVT-02 P28-EVT-01 P28-EVT-03
P28-H-01 P28-H-02 P28-H-06 P10-SAME-LEVEL-01 P10-REASSIGN-SOFTDEL-01
P11-EVT-01 P11-HP-01 P31-BL-03 P28-MEMBER-DEMOTE-01 P28-HAPPY-01
I1-DEP-02 I1-DEP-01 I1-CONC-01 I3-RACE-01 I4-DEP-02
IUM-E-02 O4-CON-01 O7-CON-01 O7-BL-16 O7-BL-17
P2-CONC-03 P6-DEP-01 P28-DEP-01 GT-E-01 P10-LEVEL-CHANGE-01
P11-CONC-01 P31-CONC-01 P31-CONC-02 P8-SEC-01
P28-TM8-RACE-01 P26-CONC-01
I1-IDP-02 I1-SEED-01 I1-RLS-01 I1-PLAN-TRIAL-01 I1-LOCALE-VAL-02
I1-SLUG-VAL-04 I1-V-05 I1-H-02 I1-H-03 I1-H-04 I1-H-05
P2-V-02 P6-SEAT-01 P6-SEAT-02 P6-409-01 P6-409-02
P7-NF-02 P28-NF-01 P28-NF-02 GT-RLS-02 GT-S-03
P10-SUSPENDED-ASSIGN-01 P10-SUSPEND-ASSIGN-01
P6-SEAT-BOUNDARY-01 P28-TARGET-NOT-MEMBER-01
I1-LOCALE-VAL-01 I1-M-01 I1-M-02 I1-V-01 I1-V-03 I1-A-03
I2-TX-02 I2-TX-01 I2-H-02 I2-RLS-01 I2-H-01 I2-CT-01 I2-A-03 I2-H-03
I3-NF-01 I2-CACHE-01 I2-HAPPY-01
I4-HP-01 I4-HP-02 I5-SEC-01 I4-EDGE-01 I4-HP-03
I4-CONC-01 I4-DEP-01 I5-DEP-02 I4-BL-02 I5-AUTH-02 I4-SEC-01
I13-VAL-04 I13-EDGE-02 I13-SEC-02
IUM-A-05 IUM-E-05
O4-A-04 O4-M-01 O4-V-01 O4-V-02 O4-V-08 O4-V-10 CROSS-03
O7-A-04 O7-BL-01 O7-BL-02 O7-BL-03 O7-BL-04 O7-BL-05 O7-BL-18 CROSS-06
P2-RLS-01 P2-NF-01 P2-NF-02 P2-M-02 P2-M-03 P2-V-07
P2-T15-01 P2-T15-02 P2-A-06
P24-422-01 P24-CONFLICT-01 P24-CACHE-01
P25-422-02 P25-CONC-01 P25-HAPPY-01
P10-422-01 P10-422-03 P10-422-02
P6-429-02 P6-429-01 P6-NF-01 P6-TRIAL-01 P6-V-03 P6-V-04
P6-IDP-02 P6-H-02 P6-H-03 P6-H-01 P6-H-04
P7-TM8-01 P7-CONC-01 P7-M-01 P7-M-04 P7-WFI-01 P7-WFI-02
P7-TM8-02 P7-TM13-01 P7-AUTH8-01
P28-GUARD-01 P28-TRIAL-01 P28-V-01 P28-M-01
GT-RLS-03 GT-A-08 GT-CF-01 GT-CF-02 GT-A-05 GT-CF-03 GT-CF-04
GT-A-02 GT-A-03 GT-A-06 GT-V-04 GT-V-03 GT-V-02 GT-S-05
P2P19-INT-01
P24-RETIRED-DEPT-01 P24-HAPPY-01 P25-ACTIVATE-RETIRED-01
P25-MEMBERS-REMAIN-01 P10-DEACTIVATED-DEPT-01
P11-HP-02 P11-SEC-01 P11-EDGE-01
P31-SEC-01 P31-VAL-03 P31-HP-01 P31-HP-02 P31-HP-03 P31-EDGE-02 P31-DEP-01
P6-REINVITE-01 P6-ROLE-VAL-01 P6-EMAIL-VAL-02 P6-HAPPY-01
P7-SUSPEND-LAST-OWNER-01 P7-STATUS-LEFT-01 P28-UNKNOWN-ROLE-01
P28RL-BL-01 P28RL-HP-02 P28RL-DEP-01 P28RL-BL-02
P26-VAL-01 P26-BL-01 P26-BL-02 P26-EDGE-01
P2-REALM-SYNC-01 P10-ROLE-DECREASE-GAP-01
I5-CONC-01 P8-DEP-02 I5-BL-01 I5-DEP-01
P11-BL-04 P11-BL-03 P11-BL-05 P11-DEP-01
P8-HP-02 P8-EDGE-01 P8-HP-01 P8-EVT-01 P8-CONC-01 P8-HP-03
P8-EVT-02 P8-BL-01 P8-DEP-01 P8-BL-02-MULTI P8-BL-03-OWN
P8-BL-04-DELEGATOR P8-DESIGN-01 P8-EVT-03
P26-FLOW-01 P26-FLOW-02 P26-DEP-02 P26-HP-02 P26-DEP-01 P26-HP-01
P26-EVT-01 P26-EVT-02 P26-EVT-03
I2-CONC-01 IUM-E-01 IUM-S-02 IUM-E-04 IUM-S-05 GT-C-05
I3-EXPIRED-INV-01 P25-MEMBERS-REMAIN-01 P28-MEMBER-DEMOTE-01
P7-IDEMPOTENT-REACTIVATE-01 I13-BL-02 O7-EVT-03 O4-RLS-01 O7-RLS-01
P9-CACHE-01 P9-CACHE-02 P3-CACHE-02 P3-CACHE-01
CRS-AUTH-01 CRS-AUTH-02 CRS-VAL-01
I11-CACHE-01 I2-AUTH-01
P12-AUTH-03 P12-AUTH-05 P12-CACHE-01 P12-CACHE-02 P12-SEC-01 P12-VAL-02
P2-LIFECYCLE-01 P2-LIFECYCLE-02
P27-AUTH-04 P27-AUTH-05 P27-AUTH-07 P27-BL-02 P27-CACHE-01
P27-EDGE-02 P27-HP-02 P27-VAL-02
P3-AUTH-05 P3-SEC-01 P3-VAL-02 P3-VAL-03
P30-AUTH-03 P30-AUTH-04 P30-AUTH-07 P30-SEC-01 P30-VAL-02
P4-AUTH-03 P4-AUTH-06 P4-BL-01 P4-BL-02 P4-BL-04 P4-BL-05
P4-CACHE-01 P4-CACHE-02 P4-EDGE-01 P4-EDGE-02 P4-EDGE-03 P4-EDGE-04
P4-HP-01 P4-HP-02 P4-HP-03 P4-HP-04 P4-HP-05 P4-HP-06
P4-SEC-01 P4-SEC-02 P4-VAL-05 P4-VAL-06
P5-AUTH-03 P5-AUTH-05 P5-CACHE-01 P5-CACHE-02 P5-SEC-01
P6-CONCURRENT-01
P7-MISSING-RV-01
P9-AUTH-03 P9-AUTH-05 P9-BL-01 P9-BL-03 P9-BL-05 P9-BL-06
P9-CACHE-03 P9-EDGE-01 P9-HP-01 P9-HP-02 P9-HP-03
""".split())

# ── Request builder ───────────────────────────────────────────────────────
def build(tc, cat, sc, method, api, exp):
    """Build (method, url, body, headers) for a test case.
    Uses the per-group TenantState to avoid cross-test contamination.
    """
    up   = tc.upper()
    scl  = sc.lower()
    catl = cat.lower()
    auth = "auth" in catl
    sec  = "security" in catl or "cross" in catl

    s = state_for(tc)  # per-group fresh tenant

    url = BASE + api
    # Support both {param} and :param URL styles
    url = re.sub(r'\{id\}',            s.tid,    url)
    url = re.sub(r':id\b',             s.tid,    url)
    url = re.sub(r'\{user_id\}',       s.member, url)
    url = re.sub(r':user_id\b',        s.member, url)
    url = re.sub(r'\{dept_id\}',       DEPT_ENG, url)
    url = re.sub(r':dept_id\b',        DEPT_ENG, url)
    url = re.sub(r'\{role_code\}',     'preparator', url)
    url = re.sub(r':role_code\b',      'preparator', url)
    url = re.sub(r'\{tender_id\}',     fresh_id(), url)
    url = re.sub(r'\{invitation_id\}', s.invite_id or NIL_UUID, url)
    # Replace {other-tenant-id} placeholder used in RLS test templates
    url = re.sub(r'\{other-tenant-id\}', s.tid, url)
    # Replace <...> placeholders (cursor, etc.) with empty/valid placeholder
    url = re.sub(r'\?cursor=<[^>]+>', '', url)    # strip invalid cursor placeholders
    url = re.sub(r'&cursor=<[^>]+>', '', url)

    hdrs = s.Hsys()
    body = None

    # ── I-2 PATCH tenant realm ──────────────────────────────────────────
    if up.startswith("I2-"):
        rv_t = s.tenant_rv()
        valid = {"realm_id": f"r-{uuid.uuid4().hex[:6]}", "realm_type":"dedicated",
                 "keycloak_shard":"eu-1", "record_version": rv_t}
        if auth:
            if exp == 401:  hdrs=H_NOAUTH; body=valid
            elif exp == 403: hdrs=s.Howner(); body=valid
            else:            body=valid           # iam-system (default)
        elif exp == 415:   hdrs=H_NOCT; body=valid
        elif exp in (400,422):
            if "realm_type" in scl or "invalid" in scl:
                body={"realm_id":"r1","realm_type":"INVALID","keycloak_shard":"eu-1","record_version":rv_t}
            elif "shard" in scl and "missing" in scl:
                body={"realm_id":"r1","realm_type":"dedicated","record_version":rv_t}
            elif "realm_id" in scl and "missing" in scl:
                body={"realm_type":"dedicated","keycloak_shard":"eu-1","record_version":rv_t}
            elif "uuid" in scl:
                url=re.sub(s.tid,"not-a-uuid",url); body=valid
            else: body={}
        elif exp == 404:   url=url.replace(s.tid,fresh_tid()); body={**valid,"record_version":1}
        elif exp == 409:   body={**valid,"record_version":9999}
        else:              body=valid

    # ── I-3 Add member ──────────────────────────────────────────────────
    elif up.startswith("I3-"):
        uid  = fresh_id()
        valid= {"user_id":uid,"email":f"{uid[:6]}@t.com","full_name":"User"}
        if auth:
            if exp==401: hdrs=H_NOAUTH; body=valid
            else:        hdrs=s.Howner(); body=valid
        elif exp==409:   body={"user_id":s.member,"email":"m@t.com","full_name":"Dup"}
        elif exp in (400,422):
            if "uuid" in scl:    body={"user_id":"bad","email":"e@t.com","full_name":"X"}
            elif "email" in scl: body={**valid,"email":"not-an-email"}
            else:                body={}
        elif exp==404:   url=url.replace(s.tid,fresh_tid()); body=valid
        else:            body=valid

    # ── I-4 Patch member status (fresh user per test) ───────────────────
    elif up.startswith("I4-"):
        # Use a fresh member each time — guarantees active state, rv=1
        fresh_uid = s.add_member()
        url = re.sub(s.member, fresh_uid, url)
        rv_u = 1
        body = {"status":"suspended","record_version":rv_u}
        if auth:
            if exp==401: hdrs=H_NOAUTH
            else:        hdrs=s.Howner()
        elif "reactivate" in scl or ("active" in scl and "suspend" not in scl):
            # Suspend first so we can reactivate
            call("PATCH", f"{BASE}/api/v1/internal/tenants/{s.tid}/members/{fresh_uid}",
                 {"status":"suspended","record_version":rv_u}, s.Hsys())
            body={"status":"active","record_version":2}
        elif "left" in scl and exp==200:
            body={"status":"left","record_version":rv_u}
        elif "cache" in scl:
            # Suspend then check cache
            body={"status":"suspended","record_version":rv_u}
        if exp in (400,422):
            if "invalid" in scl: body={"status":"godmode","record_version":rv_u}
            elif "left" in scl:  body={"status":"left","record_version":rv_u}
            else:                body={}
        if exp==404:
            url=re.sub(fresh_uid,fresh_id(),url); body={"status":"suspended","record_version":1}
        if exp==409:
            body={"status":"suspended","record_version":9999}
        if sec:
            if exp in (200,204) and "iam-system" in scl:
                # iam-system with different x-tenant-id: service overrides GUC → 200
                # Keep fresh_uid in URL (valid member of path tenant)
                hdrs=H(SYS_USER, fresh_tid(), "iam-system")
            else:
                url=re.sub(fresh_uid,fresh_id(),url)

    # ── I-5 Delete member ───────────────────────────────────────────────
    elif up.startswith("I5-"):
        if auth or sec:
            if exp==401: hdrs=H_NOAUTH
            elif exp in (200,204) and "iam-system" in scl:
                # iam-system with different x-tenant-id: service overrides GUC → 200
                tmp=s.add_member()
                url=re.sub(s.member,tmp,url)
                hdrs=H(SYS_USER, fresh_tid(), "iam-system")
            else: hdrs=s.Howner()
        elif exp==400:
            if "tenant" in scl: url=re.sub(s.tid,"not-a-uuid",url)
            else:               url=re.sub(s.member,"not-a-uuid",url)
        elif exp in (404,403):
            url=re.sub(s.member,fresh_id(),url)
        else:
            tmp=s.add_member()
            url=re.sub(s.member,tmp,url)

    # ── I-11 Internal seat usage ────────────────────────────────────────
    elif up.startswith("I11-"):
        if auth:
            if exp==401: hdrs=H_NOAUTH
            else:        hdrs=s.Howner()
        elif exp==404: url=url.replace(s.tid,fresh_tid())

    # ── I-13 Assignee override ──────────────────────────────────────────
    elif up.startswith("I13-"):
        # Ensure OWNER+MEMBER have dept assignments
        s.assign_dept(s.owner,  DEPT_ENG, "approver")
        s.assign_dept(s.member, DEPT_ENG, "preparator")
        tender_id = fresh_id()
        url = re.sub(r'/tenders/[^/]+/assignee',
                     f'/tenders/{tender_id}/assignee', url)
        valid = {"actor_id":s.owner,"new_user_id":s.member,
                 "required_level":"preparator","department_id":DEPT_ENG,
                 "tender_id":tender_id}
        if auth:
            if exp==401: hdrs=H_NOAUTH; body=valid
            else:        hdrs=s.Howner(); body=valid
        elif exp in (400,422):
            if "actor_id" in scl and "zero" in scl:
                body={**valid,"actor_id":NIL_UUID}
            elif "tender_id" in scl:
                url=re.sub(tender_id,"not-a-uuid",url); body=valid
            elif "required_level" in scl:
                body={**valid,"required_level":"manager"}
            elif "not" in scl and "member" in scl:
                body={**valid,"new_user_id":fresh_id()}
            elif "suspended" in scl:
                body={**valid,"new_user_id":s.suspended}
            elif "no dept" in scl or "dept_membership" in scl:
                # User with no dept membership
                no_dept_uid = s.add_member()
                body={**valid,"new_user_id":no_dept_uid}
            elif "department_id" in scl or "nil" in scl:
                body={**valid,"department_id":NIL_UUID}
            elif "cross" in scl or "attacker" in scl:
                body={**valid,"new_user_id":fresh_id()}
            else: body={}
        elif exp==403:
            # Actor without dept role
            no_dept_actor = s.add_member()
            body={**valid,"actor_id":no_dept_actor}
        elif exp==404:
            body={**valid,"new_user_id":fresh_id()}
        else:
            body=valid

    # ── IUM / I-8 Membership projection ────────────────────────────────
    elif up.startswith("IUM-") or up.startswith("I8-"):
        url=re.sub(r'/users/[^/?]+',f'/users/{s.owner}',url)
        if auth:
            if exp==401:   hdrs=H_NOAUTH
            elif exp==200: hdrs=s.Hsys()  # iam-system auth test → 200
            else:          hdrs=s.Hmember()
        elif exp==400: url=re.sub(s.owner,"not-a-uuid",url)
        elif exp==404: url=re.sub(s.owner,fresh_id(),url)
        if "?" not in url: url+=f"?tenant_id={s.tid}"
        if exp==400 and "tenant_id" in scl:
            url=re.sub(r'[?&]tenant_id=[^&]*','',url)
        if exp==401 and "x-tenant" in scl.replace("-",""):
            # Missing X-Tenant-ID → auth check should fail
            hdrs={"x-user-id":SYS_USER,"x-tenant-roles":"iam-system"}

    # ── GT / P-1 Get tenant ─────────────────────────────────────────────
    elif up.startswith("GT-"):
        hdrs=s.Howner()
        if auth:
            if exp==401:   hdrs=H_NOAUTH
            elif exp==403: hdrs=s.Hcross()
            elif "iam-system" in scl or "system" in scl: hdrs=s.Hsys()
            else:          hdrs=s.Hmember()
        elif exp==403:
            if "suspended" in scl: hdrs=H(s.suspended,s.tid,"member")
            elif "cross" in scl or "rls" in scl.lower():
                # iam-system with different x-tenant-id bypasses middleware but triggers handler 403
                hdrs=H(SYS_USER,fresh_tid(),"iam-system")
            else:                  hdrs=s.Hmember()
        elif exp==404:
            ntid=fresh_tid(); url=url.replace(s.tid,ntid)
            hdrs=H(SYS_USER,ntid,"iam-system")  # bypass membership check
        elif exp==401:
            hdrs=H_NOAUTH
        elif exp==301 or "trailing" in scl: url=url.rstrip("/")+"/"; hdrs=s.Howner()
        elif exp==400 or ("uuid" in scl and "invalid" in scl):
            url=re.sub(s.tid,"not-a-uuid",url); hdrs=s.Howner()
        elif "operator" in scl:             hdrs=s.Hop()
        elif "admin" in scl and exp==200:   hdrs=s.Hadmin()
        elif "cancelled" in scl or "past_due" in scl: hdrs=s.Howner()

    # ── P-2 PATCH tenant ────────────────────────────────────────────────
    elif up.startswith("P2-") or up.startswith("P2P19-"):
        hdrs=s.Howner()
        rv_t=s.tenant_rv()
        valid={"name":"Corp","record_version":rv_t}
        if auth:
            if exp==401: hdrs=H_NOAUTH; body=valid
            elif exp==403: hdrs=s.Hmember(); body=valid
            else: body=valid
        elif exp==415: hdrs=H_NOCT; body=valid
        elif exp in (400,422):
            if "59" in scl or "below" in scl:    body={"mfa_freshness_seconds":59,"record_version":rv_t}
            elif "901" in scl or "above" in scl:  body={"mfa_freshness_seconds":901,"record_version":rv_t}
            elif "30" in scl and "mfa" in scl:    body={"mfa_freshness_seconds":30,"record_version":rv_t}
            elif "locale" in scl:  body={"default_locale":"","record_version":rv_t}
            elif "broken" in scl or "malformed" in scl: body=None
            elif "empty" in scl and "body" in scl: body=None
            elif "nil" in scl and "patch" in scl:  body={"record_version":rv_t}
            elif "empty" in scl or "nil" in scl:  body={"record_version":rv_t}
            elif "uuid" in scl:    url=re.sub(s.tid,"not-a-uuid",url); body=valid
            else:                  body={"record_version":rv_t}
        elif exp==409: body={"name":"X","record_version":9999}
        elif exp==404:
            ntid=fresh_tid(); url=url.replace(s.tid,ntid)
            hdrs=H(s.owner,ntid,"tenant_owner")  # non-iam-system so RequireActiveTenant fires
            body={"name":"X","record_version":1}
        elif exp==403:
            # lifecycle — suspended/offboarded tenant blocks P-2
            hdrs=s.Hmember(); body=valid
        elif exp==202: body={**valid,"local_accounts_enabled":True}
        else:
            if "mfa" in scl and "60" in scl:    body={"mfa_freshness_seconds":60,"record_version":rv_t}
            elif "mfa" in scl and "900" in scl:  body={"mfa_freshness_seconds":900,"record_version":rv_t}
            elif "local_accounts" in scl:        body={"local_accounts_enabled":True,"record_version":rv_t}
            elif "realm" in scl or "sync" in scl: body=valid
            else:                                body=valid

    # ── P-3 List departments ────────────────────────────────────────────
    elif up.startswith("P3-"):
        hdrs=s.Howner()
        if auth:
            if exp==401: hdrs=H_NOAUTH
            elif exp in (200,201):
                if "admin" in scl: hdrs=s.Hadmin()
                elif "member" in scl or "plain" in scl: hdrs=s.Hmember()
                else: hdrs=s.Howner()
            else:        hdrs=s.Hcross()
        elif sec:
            # Cross-tenant: send caller's tenant in header, target tenant in URL path
            other=get_state("P28RL")
            hdrs=H(other.owner,other.tid,"tenant_owner")
        elif exp==403: hdrs=s.Hmember()
        elif exp==404:
            # Use non-iam-system so RequireActiveTenant fires and returns 404
            ntid=fresh_tid(); url=url.replace(s.tid,ntid)
            hdrs=H(s.owner,ntid,"tenant_owner")
        elif exp==400:
            if "zero" in scl or "00000000" in scl:
                url=re.sub(s.tid,NIL_UUID,url)
                hdrs=H(s.owner,NIL_UUID,"tenant_owner")
            else: url=re.sub(s.tid,"not-a-uuid",url)

    # ── P-4 List members ────────────────────────────────────────────────
    elif up.startswith("P4-"):
        hdrs=s.Howner()
        has_qs = "?" in url   # api template may already include query params
        if auth:
            if exp==401: hdrs=H_NOAUTH
            elif exp in (200,201):
                if "admin" in scl: hdrs=s.Hadmin()
                elif "member" in scl or "plain" in scl: hdrs=s.Hmember()
                else: hdrs=s.Howner()
            else:        hdrs=s.Hcross()
        elif sec:
            if "cursor" in scl:
                # Cursor isolation: valid same-tenant auth, alien cursor → 400 (bad cursor)
                hdrs=s.Hmember()
                if "?" not in url: url+="?cursor=alienCursorFromAnotherTenant123"
                else: url+="&cursor=alienCursorFromAnotherTenant123"
            else:
                other=get_state("P28RL")
                hdrs=H(other.owner,other.tid,"tenant_owner")
        elif "plain" in scl and "member" in scl and exp==200: hdrs=s.Hmember()
        elif exp==400 and not has_qs:
            if "cursor" in scl:   url+="?cursor=!!!notbase64"
            elif "-1" in scl:     url+="?limit=-1"
            elif "201" in scl:    url+="?limit=201"
            elif "abc" in scl:    url+="?limit=abc"
            elif "0" in scl and "limit" in scl: url+="?limit=0"
        elif exp==200 and "limit" in scl and not has_qs:
            # Only add limit param if api template doesn't already have one
            if "200" in scl and "limit=200" not in url: url+="?limit=200"
            elif "10" in scl:  url+="?limit=10"
            else:              url+="?limit=1"
        elif exp==404:
            ntid=fresh_tid(); url=url.replace(s.tid,ntid)
            hdrs=H(s.owner,ntid,"tenant_owner")

    # ── P-5 Get member ──────────────────────────────────────────────────
    elif up.startswith("P5-"):
        hdrs=s.Howner(); url=url.replace(s.member,s.owner)
        if auth:
            if exp==401: hdrs=H_NOAUTH
            elif exp in (200,201):
                if "admin" in scl: hdrs=s.Hadmin()
                elif "member" in scl or "plain" in scl: hdrs=s.Hmember()
                else: hdrs=s.Howner()
            else:        hdrs=s.Hcross()
        elif sec:
            other=get_state("P28RL")
            hdrs=H(other.owner,other.tid,"tenant_owner")
        elif exp==404: url=re.sub(s.owner,fresh_id(),url)
        elif exp==403: hdrs=s.Hcross()
        elif exp==400: url=re.sub(s.owner,"not-a-uuid",url)
        elif "member" in scl and "another" in scl and exp==200:
            hdrs=s.Hmember(); url=url.replace(s.owner,s.admin)
        elif "suspend" in scl and "cache" in scl:
            hdrs=s.Howner(); url=url.replace(s.owner,s.member)

    # ── P-7 Patch member status ─────────────────────────────────────────
    elif up.startswith("P7-"):
        hdrs=s.Howner()
        # Use fresh member to avoid contamination
        fresh_uid = s.add_member()
        url = re.sub(s.member, fresh_uid, url)
        rv_m = 1
        body = {"status":"suspended","record_version":rv_m}
        if auth:
            if exp==401: hdrs=H_NOAUTH
            elif exp==403: hdrs=s.Hmember()
            else: hdrs=s.Hmember()
        elif "reactivate" in scl or ("active" in scl and "suspend" not in scl and exp==200):
            # Suspend first
            call("PATCH", f"{BASE}/api/v1/internal/tenants/{s.tid}/members/{fresh_uid}",
                 {"status":"suspended","record_version":1}, s.Hsys())
            body={"status":"active","record_version":2}
        elif "left" in scl and exp in (200,400):
            body={"status":"left","record_version":rv_m}
        elif "last" in scl and "owner" in scl:
            # TM-8: last owner suspension → 422. Use a FRESH mini-tenant to avoid stale
            # owner grants from previous runs polluting CountActiveOwners.
            mini_owner = fresh_id()
            mini_tid = fresh_id()
            mini_h = H(SYS_USER, mini_tid, "iam-system")
            import hashlib as _hl
            mini_slug = f"m{_hl.md5(mini_tid.encode()).hexdigest()[:9]}"
            call("POST", f"{BASE}/api/v1/internal/tenants",
                 {"tenant_id":mini_tid,"slug":mini_slug,"name":"TM8","plan":"starter",
                  "licensed_seats":10,"owner_user_id":mini_owner}, mini_h)
            _, bm = call("GET", f"{BASE}/api/v1/tenants/{mini_tid}/members/{mini_owner}",
                         None, H(mini_owner, mini_tid, "tenant_owner"))
            rv_o = bm.get("record_version",1)
            url = f"{BASE}/api/v1/tenants/{mini_tid}/members/{mini_owner}"
            hdrs = H(mini_owner, mini_tid, "tenant_owner")
            body = {"status":"suspended","record_version":rv_o}
        elif "tm8" in scl.replace("-","") or "2 owner" in scl or "≥2" in scl:
            # Suspend one of ≥2 owners → 200. Use fresh mini-tenant with 2 owners.
            mini_owner1 = fresh_id(); mini_owner2 = fresh_id()
            mini_tid2 = fresh_id(); mini_h2 = H(SYS_USER, mini_tid2, "iam-system")
            import hashlib as _hl2
            mini_slug2 = f"m{_hl2.md5(mini_tid2.encode()).hexdigest()[:9]}"
            call("POST", f"{BASE}/api/v1/internal/tenants",
                 {"tenant_id":mini_tid2,"slug":mini_slug2,"name":"TM8B","plan":"starter",
                  "licensed_seats":10,"owner_user_id":mini_owner1}, mini_h2)
            # Add second user as member and grant owner
            call("POST", f"{BASE}/api/v1/internal/tenants/{mini_tid2}/members",
                 {"user_id":mini_owner2,"email":"o2@t.com","full_name":"Owner2"}, mini_h2)
            _, bm2 = call("GET", f"{BASE}/api/v1/tenants/{mini_tid2}/members/{mini_owner2}",
                          None, H(mini_owner1, mini_tid2, "tenant_owner"))
            call("PUT", f"{BASE}/api/v1/tenants/{mini_tid2}/members/{mini_owner2}/roles",
                 {"roles":["tenant_owner"],"record_version":bm2.get("record_version",1)},
                 H(mini_owner1, mini_tid2, "tenant_owner"))
            # Suspend mini_owner1 (still another active owner mini_owner2 exists) → 200
            _, bm1 = call("GET", f"{BASE}/api/v1/tenants/{mini_tid2}/members/{mini_owner1}",
                          None, H(mini_owner1, mini_tid2, "tenant_owner"))
            url = f"{BASE}/api/v1/tenants/{mini_tid2}/members/{mini_owner1}"
            hdrs = H(mini_owner1, mini_tid2, "tenant_owner")
            body = {"status":"suspended","record_version":bm1.get("record_version",1)}
        elif "missing" in scl or "zero" in scl:
            body={"record_version":0}
        if exp==409: body={"status":"suspended","record_version":9999}
        elif exp in (400,422):
            if "broken" in scl or "malformed" in scl: body=None
            elif "missing" in scl or "zero" in scl: body={"record_version":0}
            elif "uuid" in scl: url=re.sub(fresh_uid,"not-a-uuid",url)
            elif "left" in scl: body={"status":"left","record_version":rv_m}
        elif exp==404:
            url=re.sub(fresh_uid,fresh_id(),url); body={"status":"suspended","record_version":1}
        elif exp==403: hdrs=H(SYS_USER,s.tid,"tender_admin")

    # ── P-9 List dept members ───────────────────────────────────────────
    elif up.startswith("P9-"):
        s.assign_dept(s.member, DEPT_ENG, "preparator")
        hdrs=s.Howner()
        if auth:
            if exp==401: hdrs=H_NOAUTH
            elif exp in (200,201):
                if "admin" in scl: hdrs=s.Hadmin()
                elif "member" in scl or "plain" in scl: hdrs=s.Hmember()
                else: hdrs=s.Howner()
            else:        hdrs=s.Hcross()
        elif "plain" in scl and "member" in scl and exp==200: hdrs=s.Hmember()
        elif exp==400: url=re.sub(DEPT_ENG,"not-a-uuid",url)
        elif exp==404: url=url.replace(s.tid,fresh_tid())
        elif sec:
            other=get_state("P28RL")
            hdrs=H(other.owner,other.tid,"tenant_owner")

    # ── P-10 Assign dept member ─────────────────────────────────────────
    elif up.startswith("P10-"):
        hdrs=s.Howner()
        body={"level":"preparator","record_version":1}
        if auth:
            if exp==401: hdrs=H_NOAUTH
            else:        hdrs=s.Hmember()
        elif exp in (400,422):
            if "invalid" in scl and "level" in scl:
                body={"level":"invalid_level","record_version":1}
            elif "not" in scl and "member" in scl and "active" not in scl:
                url=re.sub(s.member,fresh_id(),url)
            elif "suspended" in scl and "member" in scl:
                url=url.replace(s.member,s.suspended)
            elif "dept not active" in scl or "not active for" in scl or ("not" in scl and "active" in scl and "dept" in scl):
                # Dept not activated for tenant → use DEPT_OPS (not activated by TrialSignup)
                url=re.sub(DEPT_ENG,DEPT_OPS,url)
            elif "globally" in scl or ("retired" in scl and "global" in scl) or "is_active=false" in scl:
                # Globally retired dept → use DEPT_RETIRED (is_active=false in catalog)
                url=re.sub(DEPT_ENG,DEPT_RETIRED,url)
            elif "tenant-deactivated" in scl or "deactivated" in scl:
                # Tenant-deactivated: activate DEPT_OPS then deactivate it
                call("POST", f"{BASE}/api/v1/tenants/{s.tid}/departments",
                     {"department_id":DEPT_OPS}, s.Howner())
                _, ldepts = call("GET", f"{BASE}/api/v1/tenants/{s.tid}/departments", None, s.Howner())
                rv_d = next((d.get("record_version",1) for d in ldepts.get("items",[]) if d.get("code")=="operations"), 1)
                call("PATCH", f"{BASE}/api/v1/tenants/{s.tid}/departments/{DEPT_OPS}",
                     {"is_active":False,"record_version":rv_d}, s.Howner())
                url=re.sub(DEPT_ENG,DEPT_OPS,url)
            elif "uuid" in scl and "tenant" in scl:
                url=re.sub(s.tid,"not-a-uuid",url)
            elif "uuid" in scl and "dept" in scl:
                url=re.sub(DEPT_ENG,"not-a-uuid",url)
            elif "uuid" in scl:
                url=re.sub(s.member,"not-a-uuid",url)
            else: body={}
        elif exp==404: url=re.sub(s.member,fresh_id(),url)
        elif "reviewer" in scl:  body={"level":"reviewer","record_version":1}
        elif "approver" in scl:  body={"level":"approver","record_version":1}
        elif "same" in scl:
            # Assign first then try same level
            s.assign_dept(s.member, DEPT_ENG, "preparator")
            rv_dm = 1
            _, lb = call("GET", f"{BASE}/api/v1/tenants/{s.tid}/departments/{DEPT_ENG}/members",
                         None, s.Howner())
            for item in lb.get("items", []):
                if item.get("user_id")==s.member: rv_dm=item.get("record_version",1); break
            body={"level":"preparator","record_version":rv_dm}
        elif "reassign" in scl or "softdel" in scl.replace("-",""):
            tmp=s.add_member()
            s.assign_dept(tmp, DEPT_ENG, "preparator")
            url=re.sub(s.member,tmp,url)
        elif "level" in scl and "change" in scl or "decrease" in scl or "gap" in scl:
            s.assign_dept(s.member, DEPT_ENG, "reviewer")
            rv_dm = 1
            _, lb = call("GET", f"{BASE}/api/v1/tenants/{s.tid}/departments/{DEPT_ENG}/members",
                         None, s.Howner())
            for item in lb.get("items", []):
                if item.get("user_id")==s.member: rv_dm=item.get("record_version",1); break
            body={"level":"preparator","record_version":rv_dm}
        elif "happy" in scl or "fresh" in scl or "assign" in scl:
            tmp=s.add_member()
            url=re.sub(s.member,tmp,url)
            body={"level":"preparator","record_version":1}

    # ── P-11 Remove dept member ─────────────────────────────────────────
    elif up.startswith("P11-"):
        hdrs=s.Howner()
        # Assign MEMBER to dept first
        s.assign_dept(s.member, DEPT_ENG, "preparator")
        if auth:
            if exp==401: hdrs=H_NOAUTH
            else:        hdrs=s.Hmember()
        elif sec or (exp==403 and "cross" in scl):
            other=get_state("P28RL")
            hdrs=H(other.owner,other.tid,"tenant_owner")
        elif exp==404: url=re.sub(s.member,fresh_id(),url)
        elif exp==400:
            if "tenant" in scl:  url=re.sub(s.tid,"not-a-uuid",url)
            elif "dept" in scl:  url=re.sub(DEPT_ENG,"not-a-uuid",url)
            else:                url=re.sub(s.member,"not-a-uuid",url)
        elif exp==409:
            # Simulate concurrency - assign then try to remove with wrong rv
            _, lb = call("GET", f"{BASE}/api/v1/tenants/{s.tid}/departments/{DEPT_ENG}/members",
                         None, s.Howner())
            rv_dm = 1
            for item in lb.get("items",[]):
                if item.get("user_id")==s.member: rv_dm=item.get("record_version",1)
            # Remove once to change rv
            call("DELETE", f"{BASE}/api/v1/tenants/{s.tid}/departments/{DEPT_ENG}/members/{s.member}",
                 None, s.Howner())
            # Re-assign
            s.assign_dept(s.member, DEPT_ENG, "preparator")

    # ── P-12 Role labels ────────────────────────────────────────────────
    elif up.startswith("P12-"):
        hdrs=s.Howner()
        if auth:
            if exp==401: hdrs=H_NOAUTH
            elif exp in (200,201):
                if "admin" in scl: hdrs=s.Hadmin()
                elif "member" in scl or "plain" in scl: hdrs=s.Hmember()
                else: hdrs=s.Howner()
            else:        hdrs=s.Hcross()
        elif sec:
            other=get_state("P28RL")
            hdrs=H(other.owner,other.tid,"tenant_owner")
        elif "member" in scl and exp==200: hdrs=s.Hmember()
        elif exp==403: hdrs=s.Hcross()
        elif exp==404:
            ntid=fresh_tid(); url=url.replace(s.tid,ntid)
            hdrs=H(s.owner,ntid,"tenant_owner")  # non-iam-system so RequireActiveTenant fires
        elif exp==400: url=re.sub(s.tid,"not-a-uuid",url)

    # ── P-24 Activate dept ──────────────────────────────────────────────
    elif up.startswith("P24-"):
        hdrs=s.Howner()
        body={"department_id":DEPT_OPS}
        if auth:
            if exp==401: hdrs=H_NOAUTH
            else:        hdrs=s.Hmember()
        elif exp==400:   body={}
        elif exp==409:
            # Force 409: DEPT_ENG is activated during provisioning (TrialSignup seeds it)
            body={"department_id":DEPT_ENG}
        elif exp==201:
            # First-time activation tests need a guaranteed-fresh tenant to avoid
            # 409 from previous runs. Use a mini fresh tenant inline.
            import hashlib as _p24h
            mini_owner_p24 = fresh_id()
            mini_tid_p24 = fresh_id()
            mini_h_p24 = H(SYS_USER, mini_tid_p24, "iam-system")
            mini_slug_p24 = f"p{_p24h.md5(mini_tid_p24.encode()).hexdigest()[:9]}"
            call("POST", f"{BASE}/api/v1/internal/tenants",
                 {"tenant_id":mini_tid_p24,"slug":mini_slug_p24,"name":"P24Fresh",
                  "plan":"starter","licensed_seats":10,"owner_user_id":mini_owner_p24}, mini_h_p24)
            # Choose dept based on scenario for variety
            dept_p24 = DEPT_OPS if "cache" in scl else (DEPT_LOG if ("first" in scl or "created" in scl) else DEPT_ACC)
            url = f"{BASE}/api/v1/tenants/{mini_tid_p24}/departments"
            hdrs = H(mini_owner_p24, mini_tid_p24, "tenant_owner")
            body = {"department_id":dept_p24}
        elif exp in (404,422):
            if "retired" in scl or "is_active=false" in scl or "globally" in scl:
                body={"department_id":DEPT_RETIRED}  # retired dept → 422
            else:
                body={"department_id":fresh_id()}    # non-existent → 404
        elif exp==403:   hdrs=H(SYS_USER,s.tid,"tender_admin")

    # ── P-25 Patch dept active ──────────────────────────────────────────
    elif up.startswith("P25-"):
        hdrs=s.Howner()
        # Choose the dept based on scenario
        if "system" in scl and exp==422:
            dept = DEPT_ENG   # system dept: deactivate → 422 system_cannot_be_retired
        elif "retired" in scl or ("globally" in scl and "activate" in scl):
            dept = DEPT_RETIRED  # globally retired: reactivate → 422 department_retired
        elif "member" in scl and "remain" in scl:
            dept = DEPT_LOG   # members remain after deactivation → use DEPT_LOG
        else:
            dept = DEPT_OPS   # default non-system dept for happy deactivation
        url = re.sub(DEPT_ENG, dept, url)
        # Ensure the dept is activated (for deactivation tests)
        if dept in (DEPT_OPS, DEPT_LOG, DEPT_ACC):
            call("POST", f"{BASE}/api/v1/tenants/{s.tid}/departments", {"department_id":dept}, s.Howner())
        # Get current rv from List departments (no single-dept GET route)
        _, ldepts = call("GET", f"{BASE}/api/v1/tenants/{s.tid}/departments", None, s.Howner())
        dept_code = {"de010005":"legal","de010006":"operations","de010007":"archive","de010008":"logistics","de010009":"accounting","de010001":"engineering"}.get(dept[:8], "")
        rv_d = next((d.get("record_version",1) for d in ldepts.get("items",[]) if d.get("code")==dept_code), 1)
        body = {"is_active":True,"record_version":rv_d}
        if auth:
            if exp==401: hdrs=H_NOAUTH
            else:        hdrs=s.Hmember()
        elif exp==409: body={"is_active":False,"record_version":9999}
        elif exp==400: body={"record_version":rv_d}
        elif exp==404:
            url=re.sub(dept,fresh_id(),url); body={"is_active":True,"record_version":1}
        elif exp==422:
            if "system" in scl:    body={"is_active":False,"record_version":rv_d}
            else:                  body={"is_active":True,"record_version":rv_d}  # reactivate retired → 422
        elif "deactivate" in scl or "false" in scl or ("member" in scl and "remain" in scl):
            body={"is_active":False,"record_version":rv_d}

    # ── P-27 Seat usage ─────────────────────────────────────────────────
    elif up.startswith("P27-"):
        hdrs=s.Howner()
        if auth:
            if exp==401: hdrs=H_NOAUTH
            elif exp in (200,201):
                if "admin" in scl: hdrs=s.Hadmin()
                elif "member" in scl or "plain" in scl: hdrs=s.Hmember()
                else: hdrs=s.Howner()
            else:        hdrs=s.Hcross()
        elif "admin" in scl and exp==200:  hdrs=s.Hadmin()
        elif "owner" in scl and exp==200:  hdrs=s.Howner()
        elif "member" in scl and exp==200: hdrs=s.Hadmin()  # member count test uses admin to read
        elif exp==403: hdrs=s.Hmember()
        elif exp==404:
            ntid=fresh_tid(); url=url.replace(s.tid,ntid)
            hdrs=H(s.owner,ntid,"tenant_owner")  # non-iam-system so RequireActiveTenant fires

    # ── P-28 Reconcile roles ────────────────────────────────────────────
    elif up.startswith("P28-"):
        hdrs=s.Howner()
        rv_m = s.member_rv()
        body = {"roles":["tenant_admin"],"record_version":rv_m}
        if auth:
            if exp==401: hdrs=H_NOAUTH
            else:        hdrs=s.Hmember()
        elif exp in (400,422):
            if "broken" in scl or "malformed" in scl: body=None
            elif "unknown" in scl or "superman" in scl:
                body={"roles":["superman"],"record_version":rv_m}
            elif ("member" in scl and "desired" in scl) or ("member" in scl and "derived" in scl) or ("'member'" in scl):
                body={"roles":["member"],"record_version":rv_m}
            elif "not" in scl and "member" in scl and "not" not in "invalid_owner_candidate":
                url=re.sub(s.member,fresh_id(),url); body={"roles":["tenant_admin"],"record_version":1}
            elif "last" in scl and "owner" in scl:
                # Revoke tenant_owner from last active owner → 422
                s.make_only_owner()
                url=url.replace(s.member,s.owner)
                rv_o=s.member_rv(s.owner)
                body={"roles":[],"record_version":rv_o}
            elif "uuid" in scl: url=re.sub(s.member,"not-a-uuid",url)
            else: body={}
        elif "strip" in scl or "empty" in scl or "demote" in scl:
            body={"roles":[],"record_version":rv_m}
        elif "self" in scl or "owner" in scl and "demote" in scl:
            # Only demote if there's another owner (add admin as owner first)
            rv_a=s.member_rv(s.admin)
            call("PUT", f"{BASE}/api/v1/tenants/{s.tid}/members/{s.admin}/roles",
                 {"roles":["tenant_owner","tenant_admin"],"record_version":rv_a}, s.Howner())
            url=url.replace(s.member,s.owner)
            rv_o=s.member_rv(s.owner); body={"roles":[],"record_version":rv_o}
            hdrs=s.Howner()
        elif "grant" in scl or "single" in scl or "happy" in scl:
            body={"roles":["tenant_admin"],"record_version":rv_m}
        elif "multiple" in scl:
            body={"roles":["tenant_admin","tender_admin"],"record_version":rv_m}
        elif "revoke" in scl:
            # Grant first, then revoke
            call("PUT", f"{BASE}/api/v1/tenants/{s.tid}/members/{s.member}/roles",
                 {"roles":["tenant_admin"],"record_version":rv_m}, s.Howner())
            rv_m2 = s.member_rv()
            body={"roles":[],"record_version":rv_m2}
        elif exp==403: hdrs=H(SYS_USER,s.tid,"tender_admin")
        elif exp==404:
            url=re.sub(s.member,fresh_id(),url); body={"roles":["tenant_admin"],"record_version":1}

    # ── P28RL Role labels ────────────────────────────────────────────────
    elif up.startswith("P28RL-"):
        hdrs=s.Howner()
        # Get rv from List endpoint (no single GET for role labels)
        _, lroles = call("GET", f"{BASE}/api/v1/tenants/{s.tid}/roles", None, s.Howner())
        rv_l = next((r.get("record_version",1) for r in lroles.get("items",[]) if r.get("role_code")=="preparator"), 1)
        body = {"display_name":"Custom Label","record_version":rv_l}
        if auth:
            if exp==401: hdrs=H_NOAUTH
            else:        hdrs=s.Hmember()
        elif exp==409: body={"display_name":"X","record_version":9999}
        elif exp in (400,422):
            if "empty" in scl:    body={"display_name":"","record_version":rv_l}
            elif "invalid" in scl: url=re.sub("preparator","manager_role",url)
            else: body={}
        elif exp==404:
            if "role" in scl or "label" in scl or "non-existent" in scl:
                # Non-existent role_code → 404 (same tenant, but unknown code)
                url=re.sub("preparator","nonexistent_role",url)
            else:
                ntid=fresh_tid(); url=url.replace(s.tid,ntid)
                hdrs=H(s.owner,ntid,"tenant_owner")

    # ── P-30 List invitations ────────────────────────────────────────────
    elif up.startswith("P30-"):
        hdrs=s.Howner()
        if auth:
            if exp==401: hdrs=H_NOAUTH
            elif exp in (200,201):
                if "admin" in scl: hdrs=s.Hadmin()
                elif "member" in scl or "plain" in scl: hdrs=s.Hmember()
                else: hdrs=s.Howner()
            else:        hdrs=s.Hcross()
        elif sec:
            other=get_state("P28RL")
            hdrs=H(other.owner,other.tid,"tenant_owner")
        elif "admin" in scl and exp==200:  hdrs=s.Hadmin()
        elif "owner" in scl and exp==200:  hdrs=s.Howner()
        elif exp==403: hdrs=s.Hcross()
        elif exp==404:
            ntid=fresh_tid(); url=url.replace(s.tid,ntid)
            hdrs=H(s.owner,ntid,"tenant_owner")
        elif exp==400: url=re.sub(s.tid,"not-a-uuid",url)

    # ── P-31 Revoke invitation ───────────────────────────────────────────
    elif up.startswith("P31-"):
        hdrs=s.Howner()
        inv_rv = 1
        # Each P31 test needs a fresh invitation (except 404/400/403/sec tests)
        if exp not in (400, 403, 404) and not sec:
            # Create a fresh invitation for each successful revoke test
            email = f"p31{fresh_id()[:6]}@test.com"
            c_inv, b_inv = call("POST", f"{BASE}/api/v1/tenants/{s.tid}/members",
                                {"email":email,"full_name":"P31 Invitee",
                                 "initial_tenant_role":"tenant_admin"}, s.Howner())
            if c_inv in (201,202):
                s.invite_id = b_inv.get("invitation_id") or b_inv.get("id")
                inv_rv = b_inv.get("record_version",1)
        inv = s.invite_id or NIL_UUID
        # Replace the invitation_id in URL
        url = re.sub(r'/invitations/[a-f0-9-]{36}', f'/invitations/{inv}', url)
        url = url.replace("{invitation_id}", inv)
        url = url.replace(NIL_UUID, inv) if NIL_UUID in url else url
        # P31 DELETE body: record_version required to avoid 409 optimistic lock conflict
        body = {"record_version": inv_rv}
        if auth:
            if exp==401: hdrs=H_NOAUTH
            else:        hdrs=s.Hmember()
        elif sec:
            other=get_state("P28RL")
            hdrs=H(other.owner,other.tid,"tenant_owner")
        elif exp==403: hdrs=s.Hcross()
        elif exp==404:
            url=re.sub(inv, fresh_id(), url) if inv != NIL_UUID else url
        elif exp==400:
            if "record_version" in scl and "non-integer" in scl:
                # Query-param style with invalid value
                body=None; url=url+"?record_version=notanint"
            elif "invitation_id" in scl: url=re.sub(inv,"not-a-uuid",url); body=None
            elif "tenant" in scl:        url=re.sub(s.tid,"not-a-uuid",url); body=None

    # ── O-4 Feature flags ────────────────────────────────────────────────
    elif up.startswith("O4-"):
        hdrs=s.Hop()
        rv_t=s.tenant_rv()
        body={"feature_flags":{"sso_enabled":False},"record_version":rv_t}
        if auth:
            if exp==401:   hdrs=H_NOAUTH
            elif exp==200: hdrs=s.Hop()   # operator auth test → 200 with platform_operator
            else:          hdrs=s.Howner()
        elif exp==403: hdrs=s.Howner()
        elif exp==415: hdrs=H_NOCT; body={"feature_flags":{},"record_version":rv_t}
        elif exp in (400,422):
            if "malformed" in scl or "broken" in scl: body=None
            elif "array" in scl:    body={"feature_flags":{"sso_enabled":[]},"record_version":rv_t}
            elif "object" in scl or "nested" in scl:
                body={"feature_flags":{"sso_enabled":{}},"record_version":rv_t}
            elif "unknown" in scl or "typo" in scl or "sso_enable" in scl:
                body={"feature_flags":{"sso_enable":True},"record_version":rv_t}
            elif "empty" in scl and "body" in scl: body={}
            elif "empty" in scl and "key" in scl:
                body={"feature_flags":{"":True},"record_version":rv_t}
            elif "one valid" in scl or "one unknown" in scl:
                body={"feature_flags":{"sso_enabled":True,"sso_enable":True},"record_version":rv_t}
            else: body={"feature_flags":{},"record_version":rv_t}
        elif exp==404:
            url=url.replace(s.tid,fresh_tid()); body={"feature_flags":{},"record_version":1}
        elif exp==409: body={"feature_flags":{},"record_version":9999}
        elif "malformed" in scl: body=None
        elif "rls" in scl or "x-tenant" in scl:
            # Test where x-tenant-id ≠ path param → still 200 (GUC overrides)
            hdrs=H(SYS_USER, fresh_tid(), "platform_operator")
            body={"feature_flags":{"sso_enabled":True},"record_version":rv_t}
        else:
            body={"feature_flags":{"sso_enabled":True},"record_version":rv_t}

    # ── O-7 Reassign owner ───────────────────────────────────────────────
    elif up.startswith("O7-"):
        hdrs=s.Hop()
        rv_t=s.tenant_rv()
        body={"user_id":s.admin,"record_version":rv_t}
        if auth:
            if exp==401:   hdrs=H_NOAUTH
            elif exp==200: hdrs=s.Hop()   # operator auth test → 200 with platform_operator
            else:          hdrs=s.Howner()
        elif exp==403: hdrs=s.Howner()
        elif exp==415: hdrs=H_NOCT
        elif exp in (400,422):
            # Check specific business cases BEFORE "invalid" keyword matching,
            # since scenario descriptions like "invalid_owner_candidate" contain "invalid"
            if "suspended" in scl or ("all" in scl and ("suspended" in scl or "removed" in scl)):
                body={"user_id":s.suspended,"record_version":rv_t}
            elif "left" in scl or "removed" in scl or "offboarded" in scl:
                # Create a member and set them to left status
                left_uid=s.add_member()
                call("PATCH",f"{BASE}/api/v1/internal/tenants/{s.tid}/members/{left_uid}",
                     {"status":"left","record_version":1},s.Hsys())
                body={"user_id":left_uid,"record_version":rv_t}
            elif "pending" in scl or "invited" in scl:
                inv_uid = fresh_id()
                email = f"inv{inv_uid[:6]}@test.com"
                call("POST", f"{BASE}/api/v1/tenants/{s.tid}/members",
                     {"email":email,"full_name":"Pending","initial_tenant_role":"tenant_admin"},
                     s.Howner())
                body={"user_id":inv_uid,"record_version":rv_t}
            elif "not" in scl and "member" in scl:
                body={"user_id":fresh_id(),"record_version":rv_t}
            elif "uuid" in scl or ("invalid" in scl and "uuid" in scl):
                body={"user_id":"bad","record_version":rv_t}
            elif "nil" in scl or "zero" in scl:
                body={"user_id":NIL_UUID,"record_version":rv_t}
            else: body={}
        elif exp==409: body={"user_id":s.admin,"record_version":9999}
        elif exp==404:
            url=url.replace(s.tid,fresh_tid()); body={"user_id":s.admin,"record_version":1}
        elif "deprecated" in scl or "new_owner_user_id" in scl:
            body={"new_owner_user_id":s.admin,"record_version":rv_t}
        elif "both" in scl:
            body={"user_id":s.admin,"new_owner_user_id":s.owner,"record_version":rv_t}
        elif "rls" in scl or "x-tenant" in scl:
            hdrs=H(SYS_USER, fresh_tid(), "platform_operator")
            body={"user_id":s.admin,"record_version":rv_t}
        else:
            body={"user_id":s.admin,"record_version":rv_t}

    # ── CROSS / CRS ──────────────────────────────────────────────────────
    elif up.startswith("CRS-") or up.startswith("CROSS-"):
        if "/feature-flags" in url:
            rv_t=s.tenant_rv(); hdrs=s.Hop()
            if exp==409:
                body={"feature_flags":{"sso_enabled":True},"record_version":9999}
            else:
                body={"feature_flags":{"sso_enabled":True},"record_version":rv_t}
        elif "/reassign-owner" in url:
            rv_t=s.tenant_rv(); hdrs=s.Hop()
            body={"user_id":s.admin,"record_version":rv_t}
        elif exp==400 or "content-type" in scl:
            hdrs={"x-user-id":SYS_USER,"x-tenant-id":s.tid,"x-tenant-roles":"iam-system",
                  "Content-Type":"application/xml"}
        elif exp==409:
            hdrs=s.Hop()
            body={"feature_flags":{"sso_enabled":True},"record_version":9999}  # stale → 409
        else:
            hdrs=s.Howner()

    return method, url, body, hdrs


# ── Excel styles ──────────────────────────────────────────────────────────
PASS_F=PatternFill('solid',fgColor='C6EFCE')
FAIL_F=PatternFill('solid',fgColor='FFC7CE')
SKIP_F=PatternFill('solid',fgColor='E0E0E0')
PASS_T=Font(name='Calibri',size=10,bold=True,color='1E7E34')
FAIL_T=Font(name='Calibri',size=10,bold=True,color='9C0006')
SKIP_T=Font(name='Calibri',size=10,color='666666')
CTR   =Alignment(horizontal='center',vertical='center')

def write_row(ws, r, exp, actual, passed, reason=""):
    ws.cell(r,16).value=date(2026,8,26); ws.cell(r,16).alignment=CTR
    ws.cell(r,17).value=exp;             ws.cell(r,17).alignment=CTR
    if reason:
        ws.cell(r,18).value="Manual";   ws.cell(r,18).alignment=CTR
        pf=ws.cell(r,19); pf.value="⚠ Skip"; pf.fill=SKIP_F; pf.font=SKIP_T
    elif passed:
        ws.cell(r,18).value=actual;     ws.cell(r,18).alignment=CTR
        pf=ws.cell(r,19); pf.value="✅ Pass"; pf.fill=PASS_F; pf.font=PASS_T
    else:
        ws.cell(r,18).value=actual;     ws.cell(r,18).alignment=CTR
        pf=ws.cell(r,19); pf.value="❌ Fail"; pf.fill=FAIL_F; pf.font=FAIL_T
    ws.cell(r,19).alignment=CTR


# ── Main ──────────────────────────────────────────────────────────────────
if __name__ == "__main__":
    print("Loading Excel...")
    wb  = openpyxl.load_workbook(EXCEL_PATH)
    ws  = wb[SHEET_NAME]
    ok  = fail = skip = 0
    fails = []

    for r in range(2, ws.max_row + 1):
        row = [ws.cell(r, c).value for c in range(1, 16)]
        tc  = str(row[0] or '').strip()
        if tc not in TARGET:
            continue

        cat  = str(row[1]  or '')
        sc   = str(row[2]  or '')
        meth = str(row[3]  or '').upper()
        api  = str(row[4]  or '')
        exp  = row[5]
        exp_i= int(exp) if exp else 0

        reason = skip_reason(tc, sc)
        if reason:
            write_row(ws, r, exp_i, None, False, reason)
            skip += 1
            print(f"⚠  {tc:<42} SKIP  ({reason})")
            continue

        try:
            m, url, body, hdrs = build(tc, cat, sc, meth, api, exp_i)
        except Exception as e:
            write_row(ws, r, exp_i, None, False, f"build:{e}")
            skip += 1
            print(f"⚠  {tc:<42} BUILD ERR: {e}")
            continue

        actual, resp = call(m, url, body, hdrs)

        # Capture invite_id after successful invite for P31 tests
        if tc.startswith("P31-") and actual in (200, 201, 202):
            iid = resp.get("invitation_id") or resp.get("id")
            if iid:
                s = state_for(tc)
                s.invite_id = iid

        passed = (actual == exp_i)
        write_row(ws, r, exp_i, actual, passed)

        sym = "✅" if passed else "❌"
        msg = "Pass" if passed else f"Fail(got {actual})"
        print(f"{sym} {tc:<42} exp={exp_i}  {msg}")

        if passed:
            ok += 1
        else:
            fail += 1
            fails.append((tc, exp_i, actual, sc[:50]))

        time.sleep(0.02)

    wb.save(EXCEL_PATH)

    print(f"\n{'='*68}")
    print(f"  ✅ Pass   : {ok}")
    print(f"  ❌ Fail   : {fail}")
    print(f"  ⚠ Skip   : {skip}")
    print(f"  Total    : {ok+fail+skip}")
    if fails:
        print(f"\nFailing cases:")
        for tc, exp, act, sc in fails:
            print(f"  {tc:<40} exp={exp} got={act}  {sc[:45]}")
    print(f"\nSaved → {EXCEL_PATH}")
