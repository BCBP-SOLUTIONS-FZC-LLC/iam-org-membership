# iam-org-membership — API Test Guide

Curl-based test playbook for every endpoint. Each block includes:
- **Curl** — copy-paste ready, all three headers + body inline
- **Response** — expected status + shape
- **Events** — which SQS topic(s) receive an SNS-fanned-out event
- **Common errors** — status codes to expect on misuse

Server assumed at `http://localhost:8080` (change `BASE` below if needed).

---

## Setup

### Placeholder values used throughout

| Placeholder | Value | Meaning |
|---|---|---|
| `$BASE` | `http://localhost:8080` | Server root |
| `$TENANT_A` | `11111111-1111-1111-1111-111111111111` | Test tenant A |
| `$TENANT_B` | `22222222-2222-2222-2222-222222222222` | Test tenant B (cross-tenant negatives) |
| `$ALICE` | `aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa` | User Alice — tenant_owner of A |
| `$BOB` | `bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb` | User Bob — tenant_admin of A |
| `$CAROL` | `cccccccc-cccc-cccc-cccc-cccccccccccc` | User Carol — plain member of A |
| `$DEPT_ENG` | `dddddddd-dddd-dddd-dddd-dddddddddddd` | Engineering department (global catalog) |
| `$TENDER_X` | `eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee` | Tender X |
| `$INV_ID` | `ffffffff-ffff-ffff-ffff-ffffffffffff` | Invitation row id |
| `$DEL_ID` | `12121212-1212-1212-1212-121212121212` | Delegation row id |

Export before running any curl:
```bash
export BASE=http://localhost:8080
export TENANT_A=11111111-1111-1111-1111-111111111111
export TENANT_B=22222222-2222-2222-2222-222222222222
export ALICE=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa
export BOB=bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb
export CAROL=cccccccc-cccc-cccc-cccc-cccccccccccc
export DEPT_ENG=dddddddd-dddd-dddd-dddd-dddddddddddd
export TENDER_X=eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee
export INV_ID=ffffffff-ffff-ffff-ffff-ffffffffffff
export DEL_ID=12121212-1212-1212-1212-121212121212
```

### Required gateway headers on EVERY authenticated request

| Header | Meaning |
|---|---|
| `x-user-id` | UUID of the calling user (or `iam-system` for `/internal/*`) |
| `x-tenant-id` | UUID of the tenant scope (RLS binds `SET LOCAL app.tenant_id` from this) |
| `x-tenant-roles` | Comma-separated role list — e.g. `tenant_owner,tenant_admin` |

### Watching SQS events during tests

Two outbound SNS topics fan out to per-subscriber SQS queues. Quickest check — open **http://localhost:4500** (floci-ui, started by `make docker-up`) → **Integration → SQS** and watch the **Messages** column on the queue you expect to receive the event; see README.md's "Verify event delivery in the browser" for a full walkthrough. It shows live message counts but not payloads or SNS topics/subscriptions — for those, or for scripting, use the CLI:

```bash
# List queues floci created
aws --region ap-south-1 --endpoint-url=http://localhost:4567 sqs list-queues

# Poll the membership-events workflow subscriber queue (adjust queue URL from list-queues output)
aws --region ap-south-1 --endpoint-url=http://localhost:4567 sqs receive-message \
  --queue-url http://localhost:4567/000000000000/membership-workflow-q \
  --max-number-of-messages 10 --wait-time-seconds 5

# Or peek the outbox table directly (useful for TDD):
docker exec -it iam-org-membership-postgres-1 \
  psql -U org_membership_app -d org_membership \
  -c "SELECT event_type, tenant_id, created_at FROM outbox_events ORDER BY created_at DESC LIMIT 20;"
```

---

## Infra (unauthenticated)

### GET /healthz — liveness

```bash
curl -sS -w "\nHTTP %{http_code}\n" "$BASE/healthz"
```
**Response:** `200 OK` — `{"status":"ok"}`
**Events emitted:** none

### GET /readyz — readiness (pool + cache + outbox)

```bash
curl -sS -w "\nHTTP %{http_code}\n" "$BASE/readyz"
```
**Response:** `200` when everything's up, `503` if pool/cache/outbox not ready.
**Events emitted:** none

### GET /metrics — Prometheus scrape

```bash
curl -sS "$BASE/metrics" | grep "^iam_" | head -20
```
**Response:** `200 OK` — Prometheus text format.
**Events emitted:** none

---

## Tenant routes (P-1, P-2)

### GET /api/v1/tenants/{id} — P-1: Read tenant

```bash
curl -sS -w "\nHTTP %{http_code}\n" \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_owner" \
  "$BASE/api/v1/tenants/$TENANT_A"
```
**Response:** `200 OK`
```json
{
  "id": "11111111-...", "slug": "acme", "name": "Acme Corp",
  "plan": "starter", "status": "trial", "licensed_seats": 100,
  "record_version": 1, "updated_at": "2026-07-21T..."
}
```
**Auth:** AUTH-1 — any active member of the same tenant.
**Events emitted:** none (read-only)
**Errors:** `403 insufficient_role` (cross-tenant), `404` (not visible via RLS)

### PATCH /api/v1/tenants/{id} — P-2: Update tenant

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X PATCH \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_owner" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Acme Corporation",
    "default_locale": "en-US",
    "mfa_freshness_seconds": 300,
    "local_accounts_enabled": true,
    "record_version": 1
  }' \
  "$BASE/api/v1/tenants/$TENANT_A"
```
**Response:** `200 OK` when RP realm sync succeeds; `202 Accepted` when RP is down (T-15: `realm_sync_pending=true`, reconciled by cron).
**Auth:** AUTH-1 — `tenant_owner` only.
**Events emitted:** none directly. If `local_accounts_enabled` changed and RP is called successfully, RP will emit its own realm-config event, but O&M itself emits nothing on P-2.
**Errors:** `403` (not owner), `409 optimistic_lock_conflict` (stale `record_version`), `400` (mfa_freshness outside 60..900).

---

## Department routes (P-3, P-9, P-10, P-11, P-24, P-25)

### GET /api/v1/tenants/{id}/departments — P-3: List active departments

```bash
curl -sS -w "\nHTTP %{http_code}\n" \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  "$BASE/api/v1/tenants/$TENANT_A/departments"
```
**Response:** `200 OK` — array of `{ id, code, name, is_active }`.
**Events emitted:** none

### POST /api/v1/tenants/{id}/departments — P-24: Activate a catalog department

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X POST \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  -H "Content-Type: application/json" \
  -d "{\"department_id\": \"$DEPT_ENG\"}" \
  "$BASE/api/v1/tenants/$TENANT_A/departments"
```
**Response:** `201 Created`
**Auth:** AUTH-2 — tenant_admin or tenant_owner.
**Events emitted:** none (activation only; no membership change)
**Errors:** `403`, `404` (unknown catalog department)

### PATCH /api/v1/tenants/{id}/departments/{dept_id} — P-25: Toggle is_active

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X PATCH \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  -H "Content-Type: application/json" \
  -d '{"is_active": false, "record_version": 1}' \
  "$BASE/api/v1/tenants/$TENANT_A/departments/$DEPT_ENG"
```
**Response:** `200 OK`
**Events emitted:** none
**Errors:** `422 system_department_cannot_be_retired` (blocked by `chk_system_department_active`), `409` (stale version)

### GET .../departments/{dept_id}/members — P-9: List department members

```bash
curl -sS -w "\nHTTP %{http_code}\n" \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  "$BASE/api/v1/tenants/$TENANT_A/departments/$DEPT_ENG/members"
```
**Response:** `200 OK` — `[ { user_id, level, granted_at }, ... ]`
**Events emitted:** none

### PUT .../departments/{dept_id}/members/{user_id} — P-10: Assign user to department at level

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X PUT \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  -H "Content-Type: application/json" \
  -d '{"level": "approver"}' \
  "$BASE/api/v1/tenants/$TENANT_A/departments/$DEPT_ENG/members/$CAROL"
```
**Response:** `200 OK` — echoes `{ user_id, level, record_version }`.
**Events emitted:** on `iam.membership.events`
- **`DepartmentMembershipGranted`** — first-time grant (no prior row)
- **`DepartmentMembershipLevelChanged`** — user already had a row at a different level; payload carries `previous_level`
- (no event if PUT is a no-op — same user, same level already granted)
**Errors:** `422 department_not_active_for_tenant`, `403`

### DELETE .../departments/{dept_id}/members/{user_id} — P-11: Remove user from department

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X DELETE \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  "$BASE/api/v1/tenants/$TENANT_A/departments/$DEPT_ENG/members/$CAROL"
```
**Response:** `200 OK` when the removal completes cleanly.
**Events emitted:** on `iam.membership.events`
- **`DepartmentMembershipRevoked`** — one event, payload includes `revoked_level`
**Errors:** `409 workflow_resolution_required` — §8.8.4 delegate-impact (WFI-11). Response body:
```json
{
  "code": "workflow_resolution_required",
  "active_workflows": 3,
  "workflow_ids": ["wf-1","wf-2","wf-3"],
  "allowed_actions": ["replace_delegate","stop_workflows"]
}
```
When 409, resolve via **P-26** below.

---

## Member routes (P-4..P-8, P-27, P-28)

### GET /api/v1/tenants/{id}/members — P-4: List members (cursor-paginated)

```bash
curl -sS -w "\nHTTP %{http_code}\n" \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  "$BASE/api/v1/tenants/$TENANT_A/members?limit=50"
```
**Response:** `200 OK`
```json
{ "items": [ { "user_id": "...", "status": "active", ... } ],
  "next_cursor": "eyJsYXN0X2lkIjoiLi4uIiwibGFzdF9jcmVhdGVkIjoiLi4uIn0=" }
```
**Events emitted:** none

To fetch page 2: append `&cursor=<next_cursor from page 1>`.

### GET /api/v1/tenants/{id}/members/{user_id} — P-5: Read a single member

```bash
curl -sS -w "\nHTTP %{http_code}\n" \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  "$BASE/api/v1/tenants/$TENANT_A/members/$CAROL"
```
**Response:** `200 OK` — full projection (roles + department memberships).
**Events emitted:** none

### POST /api/v1/tenants/{id}/members — P-6: Invite user (staged)

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X POST \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  -H "Content-Type: application/json" \
  -d '{
    "email": "new.user@acme.com",
    "full_name": "New User",
    "initial_tenant_roles": ["tenant_admin"],
    "initial_dept_mappings": [
      {"department_id": "'"$DEPT_ENG"'", "level": "reviewer"}
    ]
  }' \
  "$BASE/api/v1/tenants/$TENANT_A/members"
```
**Response:** `202 Accepted` — pending_invitation row created + RP `CreateInvitedUser` dispatched.
```json
{ "id": "...", "email": "new.user@acme.com", "status": "pending",
  "expires_at": "2026-07-28T...", "record_version": 1 }
```
**Events emitted:** on `iam.membership.events`
- **`TenantSeatOverageStarted`** — ONLY if this invite pushes `active + pending` above `licensed_seats` for the first time
**Errors:**
- `409 seat_limit_reached` — SEAT-1 cap (payload carries `licensed_seats`)
- `409 invitation_already_exists` — active pending row for same email
- `429 reinvite_too_soon` — PI-11 (payload `retry_after_seconds`)
- `429 invite_rate_limited` — PI-12

### PATCH /api/v1/tenants/{id}/members/{user_id} — P-7: Suspend / reactivate

```bash
# Suspend
curl -sS -w "\nHTTP %{http_code}\n" -X PATCH \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  -H "Content-Type: application/json" \
  -d '{"status": "suspended", "record_version": 1}' \
  "$BASE/api/v1/tenants/$TENANT_A/members/$CAROL"

# Reactivate
curl -sS -w "\nHTTP %{http_code}\n" -X PATCH \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  -H "Content-Type: application/json" \
  -d '{"status": "active", "record_version": 2}' \
  "$BASE/api/v1/tenants/$TENANT_A/members/$CAROL"
```
**Response:** `200 OK`
**Events emitted:** none directly. On suspend, O&M best-effort-calls RP `RevokeUserSessions` (AUTH-8 fail-open). RP itself may emit events.
**Errors:** `409` optimistic lock, `403`

### DELETE /api/v1/tenants/{id}/members/{user_id} — P-8: Remove member

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X DELETE \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  "$BASE/api/v1/tenants/$TENANT_A/members/$CAROL"
```
**Response:** `200 OK` when removal cascade completes.
**Events emitted:** on `iam.membership.events` — one event per state delta in the removal transaction
- **`DepartmentMembershipRevoked`** — one per dept the user was in
- **`TenantRoleRevoked`** — one per tenant-level role (§16 A14 multi-role)
- **`MembershipRevoked`** — one event, consumed by Delegation Service and Tender-ACL Service for their own async cascades (post-decomposition, Core owns neither table any more) and by AuthZ Enrichment for `om:memberships` cache eviction
- **`TenantSeatOverageResolved`** — if this drops the tenant back under cap
**Errors:**
- `409 workflow_resolution_required` — sync WorkflowClient.GetDelegateImpact returned >0 active workflows. Resolve via P-26 below.
- `422 last_owner_removal` — TM-8 (payload carries `owners_remaining: 1`)

### GET /api/v1/tenants/{id}/seat-usage — P-27: Seat usage projection

```bash
curl -sS -w "\nHTTP %{http_code}\n" \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  "$BASE/api/v1/tenants/$TENANT_A/seat-usage"
```
**Response:** `200 OK`
```json
{ "active_users": 42, "pending_invitations": 3, "licensed_seats": 50,
  "over_cap": false, "overage_since": null, "grace_ends_at": null }
```
**Events emitted:** none

### PUT /api/v1/tenants/{id}/members/{user_id}/roles — P-28: Reconcile roles

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X PUT \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_owner" \
  -H "Content-Type: application/json" \
  -d '{"roles": ["tenant_admin", "tender_admin"]}' \
  "$BASE/api/v1/tenants/$TENANT_A/members/$BOB/roles"
```
**Response:** `200 OK`
```json
{ "granted": ["tender_admin"], "revoked": [] }
```
**Events emitted:** on `iam.membership.events` — **one event per role delta**
- **`TenantRoleGranted`** — one per newly-granted role
- **`TenantRoleRevoked`** — one per newly-revoked role
- (no events if the desired set equals the current set)
**Errors:**
- `422 last_owner_removal` — TM-8 last owner cannot be stripped
- `400 invalid_role` — `member` is barred (TR-7 — never persisted)

### POST /api/v1/tenants/{id}/users/{user_id}/removal-resolution — P-26

Called after P-8 or P-11 returned 409 workflow_resolution_required.

```bash
# Path A — replace the delegate on all blocking workflows
curl -sS -w "\nHTTP %{http_code}\n" -X POST \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  -H "Content-Type: application/json" \
  -d "{\"action\": \"replace_delegate\", \"replacement_user_id\": \"$BOB\"}" \
  "$BASE/api/v1/tenants/$TENANT_A/users/$CAROL/removal-resolution"

# Path B — stop all blocking workflows (destructive)
curl -sS -w "\nHTTP %{http_code}\n" -X POST \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  -H "Content-Type: application/json" \
  -d '{"action": "stop_workflows"}' \
  "$BASE/api/v1/tenants/$TENANT_A/users/$CAROL/removal-resolution"
```
**Response:** `200 OK` — resolution accepted; caller must now re-issue the P-8 or P-11 DELETE.
**Events emitted:** on `iam.membership.events`
- **`DelegationEnded`** — with `ended_reason: replaced` (Path A) or `ended_reason: workflow_stopped` (Path B)
- (workflow-stop events themselves are emitted by the Workflow service, not O&M)
**Errors:** `422 invalid_replacement` — replacement user isn't an active member (WFI-5)

---

## Role-label routes (P-12, P-13)

### GET /api/v1/tenants/{id}/roles — P-12: List dept-role labels

```bash
curl -sS -w "\nHTTP %{http_code}\n" \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  "$BASE/api/v1/tenants/$TENANT_A/roles"
```
**Response:** `200 OK` — the three built-in labels (`preparator`, `reviewer`, `approver`) with their tenant-customized `display_name`.
**Events emitted:** none

### PATCH /api/v1/tenants/{id}/roles/{role_code} — P-13: Rename a dept-role label

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X PATCH \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  -H "Content-Type: application/json" \
  -d '{"display_name": "Preparateur", "record_version": 1}' \
  "$BASE/api/v1/tenants/$TENANT_A/roles/preparator"
```
**Response:** `200 OK`
**Events emitted:** none (display-only)
**Errors:** `409 optimistic_lock_conflict`

---

## Invitation routes (P-6 → members section; P-30, P-31 here)

### GET /api/v1/tenants/{id}/invitations — P-30: List pending

```bash
curl -sS -w "\nHTTP %{http_code}\n" \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  "$BASE/api/v1/tenants/$TENANT_A/invitations"
```
**Response:** `200 OK` — array of pending rows.
**Events emitted:** none

### DELETE /api/v1/tenants/{id}/invitations/{invitation_id} — P-31: Revoke

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X DELETE \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: tenant_admin" \
  "$BASE/api/v1/tenants/$TENANT_A/invitations/$INV_ID?record_version=1"
```
**Response:** `200 OK`.
Side effect: sets `kc_cleanup_pending=true` (PI-9) — durable compensation for the pending KC user. The `invitation-kc-cleanup` cron picks it up and calls RP `DeleteUser`.
**Events emitted:** on `iam.membership.events`
- **`TenantSeatOverageResolved`** — ONLY if this revoke drops the tenant back under cap

---

## Internal routes (`/api/v1/internal/*`) — `iam-system` role only

All internal routes require `x-user-id: iam-system` and `x-tenant-roles: iam-system` (mTLS auth via NetworkPolicy is the primary defence; this middleware is defense-in-depth). Tenant scope still expected via `x-tenant-id` where applicable.

### POST /api/v1/internal/tenants — I-1: Provision tenant (trial signup)

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X POST \
  -H "x-user-id: iam-system" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: iam-system" \
  -H "Content-Type: application/json" \
  -d "{
    \"tenant_id\": \"$TENANT_A\",
    \"slug\": \"acme\",
    \"name\": \"Acme Corp\",
    \"plan\": \"starter\",
    \"owner_user_id\": \"$ALICE\",
    \"default_locale\": \"en-US\"
  }" \
  "$BASE/api/v1/internal/tenants"
```
**Response:** `201 Created`.
Cascade (single tx, §8.1): tenant + 5 system depts activated + 3 role-labels seeded + owner membership + owner `tenant_owner` role.
**Events emitted:** in one atomic transaction
- On **`iam.tenant.events`**: **`TenantCreated`**, **`TrialStarted`**
- On **`iam.membership.events`**: **`TenantRoleGranted`** (owner grant), **`DepartmentMembershipGranted`** (may be emitted for owner joins if the design emits per-dept grants; see LLD §8.1)
**Errors:** `409 slug_already_taken`.

### PATCH /api/v1/internal/tenants/{id} — I-2: RP realm bind

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X PATCH \
  -H "x-user-id: iam-system" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: iam-system" \
  -H "Content-Type: application/json" \
  -d '{"realm_id":"acme-realm","realm_type":"dedicated","keycloak_shard":"kc-shard-1"}' \
  "$BASE/api/v1/internal/tenants/$TENANT_A"
```
**Response:** `200 OK`. **Events emitted:** none.

### PATCH /api/v1/internal/tenants/{id}/members/{user_id} — I-4: KC lifecycle status

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X PATCH \
  -H "x-user-id: iam-system" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: iam-system" \
  -H "Content-Type: application/json" \
  -d '{"status": "suspended", "record_version": 1}' \
  "$BASE/api/v1/internal/tenants/$TENANT_A/members/$CAROL"
```
**Response:** `200 OK`. **Events emitted:** none directly.

### DELETE /api/v1/internal/tenants/{id}/members/{user_id} — I-5: Full delete cascade

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X DELETE \
  -H "x-user-id: iam-system" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: iam-system" \
  "$BASE/api/v1/internal/tenants/$TENANT_A/members/$CAROL"
```
**Response:** `200 OK`.
Cascade (single tx): soft-delete membership, revoke all tenant roles + department memberships, set `ownerless_since` if this drops the tenant to zero owners (TM-12). Delegation/Tender-ACL cascades happen asynchronously in their own services now (they consume `MembershipRevoked` below) — Core no longer touches those tables directly.
**Events emitted:** on `iam.membership.events`
- **`DepartmentMembershipRevoked`** × depts
- **`TenantRoleRevoked`** × tenant roles
- **`MembershipRevoked`** — consumed by Delegation Service and Tender-ACL Service for their own async cascades, and by AuthZ Enrichment for `om:memberships` cache eviction
- **`TenantSeatOverageResolved`** — if this drops back under cap

### GET /api/v1/internal/users/{id}/memberships — I-8 (HOT PATH)

```bash
curl -sS -w "\nHTTP %{http_code}\n" \
  -H "x-user-id: iam-system" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: iam-system" \
  "$BASE/api/v1/internal/users/$ALICE/memberships?tenant_id=$TENANT_A"
```
**Response:** `200 OK` — full projection consumed by AuthZ Enrichment on every authenticated request. SLO: 15 ms cache-hit, 30 ms miss. Includes derived `member` role (TR-7) and `effective_feature_flags = planDefaults(plan) ⊕ tenants.feature_flags`.
**Events emitted:** none

### GET /api/v1/internal/tenants/{id}/locale — I-9

```bash
curl -sS -w "\nHTTP %{http_code}\n" \
  -H "x-user-id: iam-system" -H "x-tenant-id: $TENANT_A" -H "x-tenant-roles: iam-system" \
  "$BASE/api/v1/internal/tenants/$TENANT_A/locale"
```
**Response:** `200 OK` — `{"default_locale":"en-US"}`. **Events emitted:** none.

### GET /api/v1/internal/tenants/{id}/seat-usage — I-11 (Billing S2S)

```bash
curl -sS -w "\nHTTP %{http_code}\n" \
  -H "x-user-id: iam-system" -H "x-tenant-id: $TENANT_A" -H "x-tenant-roles: iam-system" \
  "$BASE/api/v1/internal/tenants/$TENANT_A/seat-usage"
```
**Response:** `200 OK` — same shape as P-27. **Events emitted:** none.

### POST /api/v1/internal/tenants/{id}/tenders/{tender_id}/assignee-override — I-13

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X POST \
  -H "x-user-id: iam-system" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: iam-system" \
  -H "Content-Type: application/json" \
  -d "{\"user_id\": \"$CAROL\", \"role\": \"reviewer\"}" \
  "$BASE/api/v1/internal/tenants/$TENANT_A/tenders/$TENDER_X/assignee-override"
```
**Response:** `200 OK` — validate-and-emit only (OVR-1: no persistence).
**Events emitted:** on `iam.membership.events`
- **`TenderAssigneeOverridden`** — always emitted on 200
**Errors:** `422 assignee_ineligible` (§16 A62 — deliberately 422 not 409).

---

## Operator routes (`/api/v1/operator/*`) — `platform_operator` role only

All operator routes require `x-tenant-roles: platform_operator`. `x-tenant-id` still required in header — use any UUID (usually the target tenant), but operator work is not tenant-scoped by RLS.

### PATCH /api/v1/operator/tenants/{id}/feature-flags — O-4

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X PATCH \
  -H "x-user-id: $ALICE" -H "x-tenant-id: $TENANT_A" -H "x-tenant-roles: platform_operator" \
  -H "Content-Type: application/json" \
  -d '{"feature_flags": {"beta_signature_field": true, "max_bulk_import": 500}}' \
  "$BASE/api/v1/operator/tenants/$TENANT_A/feature-flags"
```
**Response:** `200 OK`. Full-replacement per-tenant delta on top of planDefaults (PLAN-6).
**Errors:** `400 unknown_feature_flag`, `400 invalid_feature_value` (non-scalar).
**Events emitted:** none.

### POST /api/v1/operator/tenants/{id}/reassign-owner — O-7: Recover ownerless tenant

```bash
curl -sS -w "\nHTTP %{http_code}\n" -X POST \
  -H "x-user-id: $ALICE" \
  -H "x-tenant-id: $TENANT_A" \
  -H "x-tenant-roles: platform_operator" \
  -H "Content-Type: application/json" \
  -d "{\"new_owner_user_id\": \"$BOB\"}" \
  "$BASE/api/v1/operator/tenants/$TENANT_A/reassign-owner"
```
**Response:** `200 OK` — new owner granted, `ownerless_since` cleared.
**Events emitted:** on `iam.membership.events`
- **`TenantRoleGranted`** — new `tenant_owner` grant to the replacement
**Errors:**
- `422 invalid_owner_candidate` — replacement isn't an active member
- `409 tenant_offboarded` — cannot rescue a soft-deleted tenant

---

## Consumed inbound events (for context — not curl-testable)

Two SQS queues that O&M subscribes to. These aren't testable via curl but reference them when tracking event flow:

| Queue | Source topic | Events O&M consumes |
|---|---|---|
| `tenant-orgm-q` | `iam.tenant.events` (RP-produced) | `TrialTenantProvisioned`, `TenantRealmReady`, `TenantConverted`, `DirectPaidSignup`, `TrialExpired`, `TrialReactivated`, `TenantSuspended`, `TenantOffboarded` |
| `billing-orgm-q` | `billing.events` (Billing-produced) | `TenantPlanChanged`, `TenantPaymentPastDue`, `TenantSubscriptionCancelled`, `TenantReactivated`, `TenantSeatsChanged` |

Every consumed event that changes `tenants.status` or `tenants.plan` triggers an **EVT-16 relay** emit of **`TenantStateChanged`** on `iam.membership.events` in the same transaction. Guards: EVT-14 recency (stale events silently skipped but dedup-recorded), EVT-15 future-time clamp (>5 min ahead → DLQ, no dedup).

To simulate a consumed event locally, publish onto the SNS topic in floci:

```bash
# Example: simulate billing suspending a tenant
aws --region ap-south-1 --endpoint-url=http://localhost:4567 sns publish \
  --topic-arn arn:aws:sns:ap-south-1:000000000000:billing-events \
  --message '{"id":"'"$(uuidgen)"'","type":"TenantPaymentPastDue","source":"billing.events","tenant_id":"'"$TENANT_A"'","subject":"'"$TENANT_A"'","time":"2026-07-21T12:00:00Z","specversion":"1","data":{"tenant_id":"'"$TENANT_A"'"}}' \
  --message-attributes 'event_type={DataType=String,StringValue=TenantPaymentPastDue}'
```

Then verify `tenants.status = 'past_due'` and check for a `TenantStateChanged` relay in the outbox.

---

## Quick smoke-test order

For a minimal end-to-end sanity check that exercises the golden path:

1. **I-1** — provision Tenant A (owner = Alice) — `TenantCreated + TrialStarted`
2. **I-8** — assert Alice's memberships lookup works (SLO 15 ms)
3. **P-6** — Alice invites Bob — SEAT-1 cap check
4. **P-28** — Grant Bob `tenant_admin` — `TenantRoleGranted`
5. **P-10** — Add Carol to Engineering as `reviewer` — `DepartmentMembershipGranted`
6. **P-8** — Attempt to delete Bob (should be clean if no workflows) — `TenantRoleRevoked + DepartmentMembershipRevoked + MembershipRevoked`
7. **O-7** — Reassign owner if TM-12 fires

Watch the outbox table between steps:
```bash
docker exec iam-org-membership-postgres-1 \
  psql -U org_membership_app -d org_membership \
  -c "SELECT event_type, tenant_id, created_at FROM outbox_events ORDER BY created_at DESC LIMIT 20;"
```

---

## Reading events from SQS (floci)

Two ways to see what your API calls actually emitted: read from the subscriber SQS queues (real end-to-end path), or peek the outbox table (faster during dev).

### One-time shell setup

```bash
# Dummy creds so aws CLI doesn't complain when talking to floci. Region must
# match FLOCI_DEFAULT_REGION (docker-compose.yml) — floci treats region as an
# isolation boundary, so a mismatched region sees an empty queue/topic list.
export AWS_ACCESS_KEY_ID=test
export AWS_SECRET_ACCESS_KEY=test
export AWS_DEFAULT_REGION=ap-south-1
export SQS="--region ap-south-1 --endpoint-url=http://localhost:4567"
```

### 1. List queues (find the URL you want to read from)

```bash
aws $SQS sqs list-queues
```

Output shape:
```json
{
  "QueueUrls": [
    "http://localhost:4567/000000000000/tenant-orgm-q",
    "http://localhost:4567/000000000000/billing-orgm-q",
    "http://localhost:4567/000000000000/membership-workflow-q"
  ]
}
```

`tenant-orgm-q` and `billing-orgm-q` are queues **O&M reads from** (inbound consumer).
Queues subscribing to `iam.membership.events` / `iam.tenant.events` are what receive **O&M's outbound events** — poll these to verify a P-*/I-*/O-* call emitted what you expected.

### 2. Count messages waiting

```bash
QUEUE=http://localhost:4567/000000000000/membership-workflow-q

aws $SQS sqs get-queue-attributes \
  --queue-url "$QUEUE" \
  --attribute-names ApproximateNumberOfMessages ApproximateNumberOfMessagesNotVisible
```

`ApproximateNumberOfMessages` = ready to read. `…NotVisible` = in-flight (received by a consumer but not yet deleted).

### 3. Peek the next batch (does NOT delete — messages become visible again after 30 s)

```bash
aws $SQS sqs receive-message \
  --queue-url "$QUEUE" \
  --max-number-of-messages 10 \
  --wait-time-seconds 5 \
  --message-attribute-names All \
  --attribute-names All
```

Every subscription init-floci.sh creates sets `RawMessageDelivery=true` (LLD §7.3.2), so SQS's `Body` is already the plain event JSON — no SNS envelope wrapping to unwrap. Pretty-print it with `jq`:

```bash
aws $SQS sqs receive-message \
  --queue-url "$QUEUE" \
  --max-number-of-messages 10 \
  --message-attribute-names All \
  | jq -r '.Messages[] | .Body | fromjson'
```

That prints just the events — one JSON object per event with `id`, `type`, `subject`, `source`, `time`, `data`, etc.

### 4. Read AND delete (consume like a real subscriber)

```bash
MSG=$(aws $SQS sqs receive-message --queue-url "$QUEUE" --max-number-of-messages 1)
RECEIPT=$(echo "$MSG" | jq -r '.Messages[0].ReceiptHandle')
echo "$MSG" | jq -r '.Messages[0].Body | fromjson'
aws $SQS sqs delete-message --queue-url "$QUEUE" --receipt-handle "$RECEIPT"
```

### 5. Purge (clean slate between tests)

```bash
aws $SQS sqs purge-queue --queue-url "$QUEUE"
```

Note: floci rate-limits purges to one per 60 s per queue (same LocalStack-compatible behavior).

### 6. Filter by event type (SNS `event_type` message attribute)

```bash
aws $SQS sqs receive-message \
  --queue-url "$QUEUE" \
  --max-number-of-messages 10 \
  --message-attribute-names All \
  | jq -r '.Messages[]
      | select(.MessageAttributes.EventType.StringValue == "TenantRoleGranted")
      | .Body | fromjson'
```

Swap `TenantRoleGranted` for any event type you care about — `MembershipRevoked`, `TenantSeatOverageStarted`, `TenderAssigneeOverridden`, etc.

### 7. Fastest option during dev — read the outbox directly

The outbox is what `RoutingPublisher` drains into SNS. Reading it skips the SNS→SQS hop:

```bash
docker exec iam-org-membership-postgres-1 \
  psql -U org_membership_app -d org_membership \
  -c "SELECT id, event_type, tenant_id, created_at, published_at
      FROM outbox_events
      ORDER BY created_at DESC LIMIT 20;"
```

- `published_at IS NULL` → still queued locally (publisher hasn't drained it yet).
- `published_at IS NOT NULL` → already published to SNS; downstream SQS should have it within seconds.

To see the payload:
```bash
docker exec iam-org-membership-postgres-1 \
  psql -U org_membership_app -d org_membership \
  -c "SELECT event_type, jsonb_pretty(payload::jsonb) FROM outbox_events ORDER BY created_at DESC LIMIT 3;"
```

### 8. Sanity — publish a test event onto the inbound topic

To exercise the consumer + EVT-14/15/16 guards (§13), publish onto `iam.tenant.events`:

```bash
aws $SQS sns publish \
  --topic-arn arn:aws:sns:ap-south-1:000000000000:iam-tenant-events \
  --message '{
    "id":"'"$(uuidgen)"'",
    "type":"TrialExpired",
    "source":"iam.tenant.events",
    "tenant_id":"'"$TENANT_A"'",
    "subject":"'"$TENANT_A"'",
    "time":"2026-07-21T12:00:00Z",
    "specversion":"1",
    "data":{"tenant_id":"'"$TENANT_A"'"}
  }' \
  --message-attributes 'event_type={DataType=String,StringValue=TrialExpired}'
```

Then check the tenant flipped and the relay fired. `tenants` is RLS-protected (RLS-1..6) — `org_membership_app` needs `app.tenant_id` set in the same session or every row is filtered out, even ones it has a table-level grant on:
```bash
docker exec iam-org-membership-postgres-1 psql -U org_membership_app -d org_membership \
  -c "BEGIN; SET LOCAL app.tenant_id = '$TENANT_A'; SELECT id, status, last_event_at FROM tenants WHERE id = '$TENANT_A'; COMMIT;"

docker exec iam-org-membership-postgres-1 psql -U org_membership_app -d org_membership \
  -c "SELECT event_type, tenant_id, created_at FROM outbox_events WHERE event_type='TenantStateChanged' ORDER BY created_at DESC LIMIT 3;"
```
