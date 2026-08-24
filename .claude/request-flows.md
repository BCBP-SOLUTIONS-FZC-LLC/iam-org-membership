# Request Flows, Concurrency & GDPR

## 8.1 Tenant Provisioning — Trial Signup

Signup BFF → `POST /internal/tenants` (I-1). One `RunInTx`:
1. `INSERT tenants ON CONFLICT DO NOTHING`
2. `INSERT tenant_departments` for 5 system departments
3. `INSERT dept_role_labels` for 3 default role labels (preparator/reviewer/approver)
4. `INSERT tenant_memberships` (owner, `status='active'`)
5. `INSERT tenant_roles` (owner, `tenant_membership_id=<owner membership id>`, `role_code='tenant_owner'`, `granted_by=owner_user_id`) — I1-3, TM-8, TR-8
6. `outbox.Enqueue(TenantCreated)` + `outbox.Enqueue(TrialStarted)`
7. COMMIT

Returns `201 {tenant_id}`. Outbox runner publishes both events to `iam.tenant.events`.

## 8.2 Trial → Paid Conversion

Realm Provisioner emits `TenantConverted` on `iam.tenant.events`. O&M consumes via `tenant-orgm-q`:
```
RunInTx: UPDATE tenants SET status='active', subscription_started_at=now(), plan='starter'
         WHERE id=$1 AND record_version=$2
```
`feature_flags` untouched (T-9). No `plan_quotas` write (§16 A26 out of scope). Evict `om:tenant:{id}`, `om:locale:{id}`. AuthZ Enrichment independently consumes the same event to refresh its own plan-flag cache.

## 8.3 AuthZ Enrichment Hot Path (I-8)

```
Envoy → AuthZ.CheckRequest
  → AuthZ.Valkey GET ae:ctx:{tenant}:{user}
    hit → return
    miss → O&M I-8
      → O&M.Valkey GET om:memberships:{tenant}:{user}
        hit → return
        miss → Postgres single joined query → SET om:memberships:{tenant}:{user} TTL=300s±jitter
      → AuthZ.SET ae:ctx:{tenant}:{user} TTL=60s
  → OKResponse {x-tenant-id, x-user-id, x-tenant-roles, x-plan, ...}
```

SLO: 15 ms p99 cache hit, 30 ms p99 cache miss. Derived `member` role always injected at projection layer.

## 8.4 User Added to Department (P-10 upsert)

```
Validate: user has active tenant_membership (DM-2) and dept active for tenant (D-5/TD-6)
RunInTx: SELECT current dept_membership (incl. soft-deleted) then UPSERT SET role_level, deleted_at=NULL
  Event selection (state-dependent):
    new membership OR reactivated (prior deleted_at NOT NULL) → DepartmentMembershipGranted
    existing active + role_level changed → DepartmentMembershipLevelChanged {previous_level, new_level}
    existing active + role_level unchanged → NO EVENT (TRG-3 no-op, no record_version bump)
COMMIT → DEL om:memberships:{tenant}:{user}, om:dept_members:{tenant}:{dept}
```

## 8.5 SAML Group Assertion → JIT

Event Consumer → `POST /internal/tenants/:id/dept-memberships` (I-10) with `{user_id, groups[]}`.

Resolution (ADR-0007 Wave 2 — no longer a local join; `group_dept_mappings`/
`group_dept_role_mappings`/`group_tenant_role_mappings` now live in Group
Mapping Service):
```
MGET om:grm:{tenant}, om:gdm:{tenant}, om:gtrm:{tenant}
  all 3 hit → use cached resolution
  any miss → GroupMappingClient.ResolveGroups(tenant, groups[]) [GM-I1, mesh-only]
    success → SET om:grm/gdm/gtrm TTL=600s + om:*:stale TTL=24h, use resolution
    failure → MGET om:grm:stale/gdm:stale/gtrm:stale
      all 3 hit → use stale resolution (tagged distinctly for observability)
      miss → fail OPEN: empty resolution, I-10 still returns 200 (never fail the SAML login)
RunInTx:
  For each resolved (dept, role_level) → UPSERT dept_memberships (same rule as §8.4)
  For each resolved role_code → INSERT tenant_roles ON CONFLICT DO NOTHING (GTRM-4, additive-only)
    (granted_by = iam-system)
COMMIT → DEL om:memberships, DEL om:dept_members per affected dept
```

**Additive-only rule (GTRM-4):** JIT **never revokes** — a group no longer including a previously-granted role does NOT remove the grant. Revocation is explicit admin action only (P-28). Mirrors DM-1/SEAT-3/DEL-5's passive-trigger philosophy.

## 8.6 Delegation — Moved to Delegation Service

The full OOO delegation lifecycle previously documented in this section and §8.7 — create with User Profile availability coordination, the 5-minute expiry sweep, the `delegation-review` CronJob's review-window nudges, and P-32/P-33 extend/reassign — no longer exists in this repo. It moved in its entirety to the standalone **Delegation Service** (`iam-delegation`, ADR-0008), along with the `delegations` table (+ its two ENUMs) and `tenants.delegation_max_duration_days`/`delegation_review_window_days`. The `port.UserProfileClient`/`userprofile` outbound adapter that this flow used is also gone — it became dead code (zero remaining call sites) once the coordination flow left, and has been deleted (LLD §16 OQ-5). See the Delegation Service's own LLD for its equivalent flow documentation.

**Core's only remaining delegation touchpoints** are both synchronous, read-only, and off the I-8 hot path:
1. The delegate-impact gate on user removal (§8.8) via `port.WorkflowClient` — this talks to the **Workflow** Service, not Delegation, and is completely unaffected by the decomposition.
2. The department-scope precision lookup on the admin dept-membership Assign/Remove path (§8.8.4) via `port.DelegationCheckClient` → `GET {DELEGATION_BASE_URL}/internal/delegations/dept-delegate`, which replaces the local `delegations` table lookup Core lost. On a Delegation Service outage this degrades to tenant-wide impact scoping (still correct, less precise) — never a hard failure.

## 8.7 (removed — see §8.6)

Delegation expiry and the review-window flow formerly documented here moved to the Delegation Service along with the rest of the delegation lifecycle.

## 8.8 User Removal — Delegate-Impact Resolution (resolves §16 C2)

**Problem:** removing a user could strand active workflows routed to them as delegate.

**Solution:** synchronous `port.WorkflowClient.GetDelegateImpact` pre-check before removal cascade. If `active_workflows > 0`, refuse `409 workflow_resolution_required`. Admin resolves via P-26.

### 8.8.1 `port.WorkflowClient`

```go
type WorkflowClient interface {
    GetDelegateImpact(ctx, tenantID, delegateUserID uuid.UUID, delegationID *uuid.UUID) (DelegateImpact, error)
    ReassignDelegate(ctx, tenantID, oldDelegateID, newDelegateID uuid.UUID, delegationID *uuid.UUID) (ReassignResult, error)
    CancelByDelegate(ctx, tenantID, delegateUserID uuid.UUID, delegationID *uuid.UUID) (CancelResult, error)
}
```

**Request shape (§16 A12 confirmed with Workflow Service):**
- `GET /internal/workflows/delegate-impact?tenant_id=&delegate_user_id=&delegation_id=` — query params, NOT JSON body on GET.
- `POST /internal/workflows/reassign-delegate` — body `{tenant_id, old_delegate_id, new_delegate_id, delegation_id?}`.
- `POST /internal/workflows/cancel-by-delegate` — body `{tenant_id, delegate_user_id, delegation_id?}`.

`delegation_id`: nil = tenant-wide (§8.8 full removal); non-nil = scoped to that specific delegation (§8.8.4 dept-level). Workflow Service already tags task assignments `reason="delegation:<id>"`.

HTTP adapter: `adapter/outbound/workflow/http_client.go`, `gincommon.PropagateHeaders`, `WORKFLOW_SERVICE_TIMEOUT_MS` (default 3000).

### 8.8.2 DELETE Pre-check

```
DELETE /tenants/:id/members/:user_id (P-8) OR /internal/tenants/:id/members/:user_id (I-5)
  → WorkflowClient.GetDelegateImpact(tenant, user, delegation_id=nil)
      5xx/timeout → 503 workflow_service_unavailable (no DB write, WFI-8)
      active_workflows > 0 → 409 workflow_resolution_required
        body: {active_workflows, delegate_user_id, workflow_ids[], allowed_actions: [replace_delegate, stop_workflows]}
        iam_delegate_removal_blocked_total{trigger=full_removal}++
        NO membership/delegation change, NO event (WFI-3)
      active_workflows == 0 → proceed to existing §15.2.2 cascade
```

### 8.8.3 P-26 Resolution Endpoint

`POST /tenants/:id/users/:user_id/removal-resolution {action, replacement_user_id?}`

**action=`replace_delegate`:**
1. Pre-validate `replacement_user_id` is active member same-tenant → else `422 invalid_replacement` (WFI-5) **before** any Workflow call.
2. `WorkflowClient.ReassignDelegate(tenant, oldDelegateID=userID, newDelegateID=replacementID)` → `iam_delegate_reassignment_total++`.
3. Re-invoke `GetDelegateImpact` for race-safety re-check (WFI-6). If still `> 0` (a new workflow attached concurrently) → `409 workflow_resolution_required` again — admin resubmits.
4. Apply the §15.2.2 removal cascade.

**action=`stop_workflows`:**
1. `WorkflowClient.CancelByDelegate(tenant, userID)` → `iam_delegate_workflow_cancel_total++`.
2. Re-check + cascade as above.

**On cascade:** Core no longer owns a `delegations` table or a delegation-specific event. It emits a single `MembershipRevoked{tenant_id, user_id, actor_id}` (LLD §15.2.2, unconditional — not gated on whether the user held any delegation rows), consumed asynchronously by the Delegation Service (which ends this user's delegation rows, including any where they were delegate) and the Tender-ACL Service (which soft-deletes their ACL overlay rows). See §8.8's cascade step and §15.2 for the full event.

### 8.8.4 Department-Level Extension (P-10 decrease / P-11)

Same gate, now backed by the Delegation Service's dept-delegate lookup instead of a local `delegations` table query (ADR-0008 §6.4 — Core lost that table):
- **Pre-filter:** `port.DelegationCheckClient.DeptDelegate(tenant, user, dept)` → `GET {DELEGATION_BASE_URL}/internal/delegations/dept-delegate` returns the id of the active `scope='department'` delegation where this user is delegate for `(user, dept)`, or none. On a Delegation Service error/timeout this degrades to a nil id (`deptDelegateOrDegrade`) rather than failing the request — Assign/Remove is never blocked by this dependency.
- **P-10 promotion or unchanged** → check is **skipped entirely** (WFI-12, mock `AssertNotCalled`).
- **P-10 decrease** → if the pre-filter yields no id (none found, or degraded on outage) → proceed unchanged, `WorkflowClient.GetDelegateImpact` is not called. If it yields an id → `WorkflowClient.GetDelegateImpact(tenant, user, delegation_id=<id>)`.
- **P-11** → always calls `WorkflowClient.GetDelegateImpact(tenant, user, delegation_id)` with whatever the pre-filter returned — a specific id, or nil (which the Workflow Service's contract treats as tenant-wide, §8.8.1 — still correct, less precise, never a hard failure).
- `active_workflows > 0` → `409 workflow_resolution_required` (`trigger=dept_demotion`/`dept_removal`).
- `scope='all'` delegations remain outside this dept-scoped pre-filter (WFI-10) — they still gate full removal (§8.8), not dept demotion.

### 8.8.5 Suspension Advisory (§16 C3, WFI-13, fail-open)

P-7 suspend calls `GetDelegateImpact` **best-effort**:
- Suspend **still commits** regardless of Workflow response (fail-open — never blocks a security-relevant suspend).
- Response includes `delegate_impact.active_workflows` (or `checked:false` if RP 5xx).
- No `409` ever returned.
- Delegation row **untouched** (frozen, not ended — M-1 suspension freezes state).
- `iam_delegate_suspend_impact_total` metric + `delegate_suspend_impact` INFO log if `active_workflows > 0`.
- P-7 **reactivate** (`suspended → active`) never calls `GetDelegateImpact`.

## 8.10 Two-Step Invite → Accept (§16 A11)

```
Admin → POST /tenants/:id/members {email, full_name, initial_tenant_roles, initial_dept_mappings} (P-6)
  Pre-flight:
    active member for this email? → 409 member_already_exists
    pending invite for this email? → 409 invitation_already_exists (PI-1)
    per-email cooldown active? → 429 reinvite_too_soon (PI-11, iam_invite_throttled_total{cooldown})
    per-tenant hourly ceiling? → 429 invite_rate_limited (PI-12)

  RealmProvisioner.CreateInvitedUser(tenantID, {email, required_actions:[VERIFY_EMAIL, UPDATE_PASSWORD, CONFIGURE_TOTP?]})
    5xx/timeout → 503 realm_provisioner_unavailable (retryable)
    → keycloak_user_id

  RunInTx: SELECT tenants.licensed_seats FOR UPDATE + count active + pending
    over-cap? → COMMIT a revoked row (keycloak_user_id, kc_cleanup_pending=true) for durability
                → 409 seat_limit_reached (SEAT-1), reconciler deletes KC user (PI-9)
    else → INSERT pending_invitations (status=pending, expires_at=now()+INVITATION_EXPIRY_DAYS, keycloak_user_id)
           + InvitationCreated audit
  COMMIT → 202 {invitation_id, status, expires_at}

Keycloak: user clicks link → verify email, set password, enroll MFA
Keycloak → Event Consumer: REGISTER + VERIFY_EMAIL + UPDATE_PASSWORD webhook
Event Consumer → POST /internal/tenants/:id/members {user_id, email} (I-3)
  RunInTx: match pending row FOR UPDATE (idx_pi_keycloak_user)
    → UPDATE pending_invitations SET status=accepted, accepted_at=now()
    → INSERT tenant_memberships (active) — the member grant itself (TR-7, no member row)
    → INSERT initial_tenant_roles (elevated only — TenantRoleGranted per row)
    → INSERT initial_dept_mappings (DepartmentMembershipGranted per row)
  COMMIT → DEL om:memberships, om:members, om:seat_usage
  Outbox → SNS: TenantRoleGranted + DepartmentMembershipGranted → iam.membership.events
```

**Seat-hold coupling (T-8/SEAT-1):** `INVITATION_EXPIRY_DAYS = 7` must equal Keycloak invite action-token lifespan (HLD §8.2.2's 7-day link) — divergence would either strand a seat past a dead link or free a seat while the link still works.

**Revoke/expiry (PI-5/PI-6):** P-31 revoke → `status=revoked` AND `kc_cleanup_pending=true` **atomically**; `invitation-expiry` CronJob past `expires_at` → same. Both trigger the `invitation-kc-cleanup` reconciler (PI-9) which idempotently calls `RealmProvisioner.DeleteUser`. There is no monthly hard-delete job for terminal `pending_invitations` rows in this repo — no `invitation-cleanup` CronJob exists (see §15.7); terminal rows persist until tenant offboarding cascade or an explicit GDPR erasure-by-email (§15.8).

## 9. Concurrency, Consistency, Failure

### 9.1 Optimistic Locking

```go
tag, err := tx.Exec(ctx,
    "UPDATE tenant_memberships SET status=$1 WHERE id=$2 AND tenant_id=$3 AND record_version=$4",
    newStatus, id, tenantID, expectedVersion,
)
if tag.RowsAffected() == 0 {
    return domain.ErrConflict  // → 409 optimistic_lock_conflict
}
```

Canonical vocabulary: `optimistic_lock_conflict` / `record_version` (API-3, TM-10, §5.5, §17).

### 9.2 Idempotency

- Outbound: UUID v7 `id` in envelope, consumer `processed_events` dedup, outbox `ClaimLease` prevents double-publish.
- Internal: `ON CONFLICT DO NOTHING` (tenant) / `DO UPDATE` (memberships), Event Consumer sends stable `idempotency_key` from Keycloak event ID.
- JIT: `INSERT ... ON CONFLICT (tenant_id, user_id, department_id) DO UPDATE SET role_level=…, deleted_at=NULL`.

### 9.3 Failure Scenarios

| Scenario | Detection | Recovery |
|---|---|---|
| DB commit OK, Valkey DEL fails | logged | TTL self-heals (≤300 s) |
| Outbox crashes after publish, before mark | lease expires, re-claimed | Consumer `processed_events` dedups |
| DELETE cascade invoked twice | `processed_events` insert-or-ignore | Second is no-op |
| RLS GUC unset | `rls_check_tenant` slow path → violation log | 0 rows, CloudWatch alarm |
| SNS throttle during outbox publish | platform-events retryable | Auto retry; DLQ on permanent failure |

**Failure invariants (FAIL-1..5):**
- **FAIL-1** — Dependency failures never leave partial business state (single `RunInTx` all-or-nothing).
- **FAIL-2** — Cache failures degrade latency only.
- **FAIL-3** — Outbox at-least-once; consumer dedup = exactly-once at consumer.
- **FAIL-4** — Scheduled jobs safe to restart/re-run.
- **FAIL-5** — Missing tenant context fails closed (RLS-2).

### 9.4 Consistency (CONS-1..4)

- **CONS-1** — Business write + integration event(s) committed together via transactional outbox (EVT-10). No event without state; no state without event.
- **CONS-2** — *(retired from Core, ADR-0008)* Availability-first delegation coordination (`delegations` updated only after User Profile's `200`) now lives entirely in the Delegation Service — Core has no `delegations` table, no `UserProfileClient`, and no write path to protect. See that service's own LLD for its consistency invariants.
- **CONS-3** — JIT membership per-request atomic. All resolved `(dept, role)` in one `RunInTx`; no partial assignment.
- **CONS-4** — Advisory display vs transactional hard limit. Seat cap enforced transactionally with `SELECT ... FOR UPDATE`, never from `om:seat_usage` cache.

## 15. GDPR & Data Lifecycle

### 15.1 Scenario → Workflow Reference

| Scenario | O&M subsection | Workflow doc |
|---|---|---|
| User deletion & role demotion | §15.2 | `IAM HLD/user-deletion-role-demotion-workflow.md` |
| Trial expiry & cleanup | §15.3 | `IAM HLD/trial-expiry-cleanup-workflow.md` |
| Trial reactivation | §15.4 | `IAM HLD/trial-reactivation-workflow.md` |
| Tenant offboarding (paid) | §15.5 | `IAM HLD/tenant-offboarding-workflow.md` |

### 15.2 User Deletion

**Cross-service pattern:** Keycloak hard-deletes; User Profile scrubs its per-user PII (`display_name`, `phone`, `job_title`, `credentials`, signature, availability); O&M sets membership `status='left'` + `deleted_at`; cascade soft-deletes `tenant_roles`/`dept_memberships` (the two tables Core still owns) and emits one `MembershipRevoked{tenant_id, user_id, actor_id}` (LLD §15.2.2). O&M no longer owns `delegations`/`tender_acl_entries` and does not touch them directly — the Delegation Service and Tender-ACL Service each run their own async cascade off that shared `MembershipRevoked` signal, ending delegation rows and soft-deleting ACL overlays respectively in their own databases. (This used to be two separate emissions — `MembershipRevoked` plus a `TenantMembershipRemoved` aimed at Tender-ACL — now consolidated into the one shared event per the LLD.)

**§16 A45 / TR-9:** removal soft-deletes ALL `tenant_roles` rows for the user (symmetric with dept_memberships); one `TenantRoleRevoked` emitted per revoked elevated grant. Suspend (P-7) leaves `tenant_roles` untouched (frozen, M-1).

**§16 A44 / TM-13 last-owner concurrency:** `SELECT ... FOR UPDATE` on tenants row serializes concurrent owner-drops. Two concurrent P-28 revokes of the last two owners: exactly one succeeds, the other gets `422 last_owner_removal`.

**§15.2.3 same user registers again:** old rows retained with `deleted_at`; new registration gets a **new** `user_id`. `uq_tm_active_user`'s `WHERE deleted_at IS NULL` allows rejoin. No DB-level link between old and new identities.

### 15.3 Trial Expiry & Cleanup

**Ownership split (do not conflate):** Realm Provisioner owns realm-side sweep (detects expiry, disables/deletes users in shared trial realm, emits `TrialExpired`). O&M owns DB-side transitions.

**Phase 1 (event-driven):** RP emits `TrialExpired` → O&M consumes (`tenant-orgm-q`) → `status='trial_expired'` (audit-logged). **No PII scrub yet** — reactivatable during 15-d grace.

**Phase 2 (after 15-d grace, `trial_ends_at < now() - 15d`):**
1. RP hard-deletes Keycloak users; each `USER_DELETE` flows through Event Consumer to O&M (I-5) and User Profile (UP scrubs per-user PII per UP LLD §8.7).
2. O&M's `trial-cleanup` CronJob (§13.1) soft-deletes `tenants` row + scrubs tenant-level PII + retains `id` for audit FK. `status` stays `trial_expired` (NOT `offboarded`).
3. Audit Log entries retained (§15.6 schedule).
4. `trial_signup_ledger` retained (email hash only, no PII; enforces one-lifetime-trial HLD Invariant TRIAL-2/TRIAL-6).

Retention boundary anchored to `trial_ends_at + 15 days` — deterministic. Sweeps idempotent.

### 15.4 Trial Reactivation (HLD TRIAL-5, T-14)

Within 15-d grace, `trial_expired` reactivatable **once** via signed link. RP validates single-use token, re-enables user, emits `TrialReactivated`. O&M consumes (`tenant-orgm-q`) and applies in **one tx**:
1. `status='trial'` (audit `TrialReactivated`).
2. **Fresh window:** `trial_ends_at = now() + plan.trial_duration_days` (HLD Invariant TRIAL-7, monotonic).
3. Increment `trial_reactivation_count` (T-14 CHECK backstop caps at 1).

Layered one-time guarantee: (a) single-use token (RP redeems), (b) counter check + T-14 CHECK, (c) `processed_events` dedup.

### 15.5 Tenant Offboarding (Paid)

Billing owns status transitions; O&M and RP react. Sequence `cancelled → suspended → offboarded`:
- **`cancelled`** (default 30 d): read-only grace, `cancelled_at` set (drives clock).
- **`suspended`** (grace elapsed): RP disables dedicated realm, no login, data retained.
- **`offboarded`** (retention elapsed, default 90 d from `cancelled_at`): RP exports realm to encrypted S3 then hard-deletes; O&M soft-deletes tenants row + scrubs PII + retains `id`. **Terminal (PAID-1).** Reactivation possible **only before offboarding**.

**§16 A54 / OFF1 fan-out contract:** RP publishes `TenantOffboarded` once (only after export+delete verified). Consumers: O&M (wipes tenant-scoped rows), User Profile (scrubs per-user PII UP LLD §8.7a), Audit Log (retains per §15.6).

O&M wipe on `TenantOffboarded`:
1. `UPDATE tenants SET status='offboarded', deleted_at=now(), <PII scrubbed>`.
2. `ON DELETE CASCADE` propagates to O&M's own retained tenant-scoped tables.
3. Emit `TenantMembershipsPurged{tenant_id, actor_id}` on `iam.membership.events` so the Delegation, Tender-ACL, and Group-Mapping services — whose tables live in separate databases, out of reach of O&M's own cascade — can run their own tenant-scoped cascade-deletes.
4. Invalidate `om:*:{tenant_id}:*` Valkey keys (CACHE-8).

**Naming note:** `TenantMembershipsPurged` was previously named `TenantOffboarded` and routed on `iam.tenant.events` — that was wrong, since `TenantOffboarded` is Realm Provisioner's own terminal event, which O&M only *consumes*; having O&M also emit an event of the same name would violate one-producer-per-event-name. This pass renamed O&M's own fan-out signal to `TenantMembershipsPurged` on `iam.membership.events`. O&M's *consumption* of RP's real `TenantOffboarded` (step 1 above) is unchanged.

Idempotent via `processed_events`. The whole wipe (steps 1–3) commits as one transactional-outbox unit; O&M never re-emits RP's `TenantOffboarded` verbatim, only its own distinctly-named `TenantMembershipsPurged` downstream signal.

### 15.7 Data Retention (Operational)

| Table | Retention | Mechanism |
|---|---|---|
| `processed_events` | 8 d | `processed-events-prune` CronJob (IDEMP-4 window; > 7-d SQS lifetime) |
| `rls_violation_log` | 30 d | Hourly CronJob |
| `outbox_events` (published) | Daily prune | `outbox-prune` CronJob (`outbox.Runner.PrunePublished`) |
| `pending_invitations` (terminal) | not hard-deleted by any job | No `invitation-cleanup` CronJob exists in this repo — terminal rows (`accepted`/`expired`/`revoked`) persist until tenant offboarding cascade (`ON DELETE CASCADE`) or an explicit GDPR erasure-by-email (§15.8) |

**Removed from this table (ADR-0008/ADR-0007):** `delegations` and `tender_acl_entries` retention rows — Core owns neither table any more; their soft-delete-then-hard-delete retention is now the Delegation Service's and Tender-ACL Service's own concern in their respective databases.

### 15.8 PII Boundary

O&M stores **no PII beyond opaque UUIDs for members** — `user_id` (Keycloak sub) and `tenant_id`. Member PII (name, email, credentials) owned by User Profile + Keycloak, scrubbed at those layers on deletion.

**One exception — `pending_invitations` (§16 A38):** invitee has no Keycloak/UP identity yet, so `pending_invitations` holds `email` (citext) + `full_name` until acceptance. Erasure handling:
- **Tenant offboarding** — `fk_pi_tenant ... ON DELETE CASCADE` scrubs.
- **Terminal rows** — no automated hard-delete CronJob exists for these in this repo (§15.7); they persist until tenant offboarding cascade or explicit person-level erasure below.
- **Person-level GDPR erasure** for an invited-but-never-accepted person — scrub `pending_invitations` **by email** (`citext` case-insensitive). Explicit erasure-runbook step; no `user_id` exists. Any not-yet-activated Keycloak shell via `kc_cleanup_pending` (PI-9).

**Only table whose GDPR treatment is keyed on email, not `user_id`.**
