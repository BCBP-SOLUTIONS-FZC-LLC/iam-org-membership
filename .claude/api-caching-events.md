# API, Caching & Events

## 5.1 API Conventions

- All routes under `/api/v1`. Breaking changes ship under `/api/v2`. Endpoint catalogue below writes paths **in full** (`/api/v1/...`).
- Middleware: `gincommon.DefaultMiddlewares` (`PanicRecovery → RequestID → Tracing → CorrelationHeaders → Metrics → Logging → RequireAuth → ContextMiddleware`) + GUC-bridge.
- **Three route prefixes with distinct auth models:**

| Prefix | Callers | Auth | Ingress |
|--------|---------|------|---------|
| `/api/v1/*` | Authenticated tenant users | Gateway-injected `x-user-id`, `x-tenant-id`, `x-tenant-roles` from validated JWT | Public Envoy |
| `/api/v1/internal/*` | In-mesh services (Realm Provisioner, Event Consumer, LLM, Signup BFF, Workflow, Billing, AuthZ) | Mesh mTLS + NetworkPolicy; **no JWT**; `x-tenant-id` = target tenant | Internal Envoy only; external blocked |
| `/api/v1/operator/*` | Human operators / operator tooling | Gateway validates JWT, asserts `platform_operator` Keycloak role | **Separate operator ingress** — unreachable from public tenant Envoy (§16 C1 defense-in-depth) |

- Content type `application/json`. Timestamps RFC 3339 UTC. Errors via `gincommon.ErrorResponse`.
- Mutation responses echo `record_version` + `updated_at` for optimistic-lock round-tripping.
- Input validation: `slug` regex `^[a-z0-9][a-z0-9-]{2,62}[a-z0-9]$`; `default_locale` BCP-47; enums; `keycloak_group_name` ≤ 200 chars matching `^[a-zA-Z0-9_./-]+$`; UUIDs at handler layer.

**API invariants (API-1..3):**
- **API-1** — Never trust body-supplied `tenant_id`/`user_id`/roles. Identity from headers only.
- **API-2** — Path/body `tenant_id` never widens access — RLS returns 0 rows/`WITH CHECK` rejects on mismatch.
- **API-3** — Mutations round-trip `record_version`; DB-managed via `touch_row` (TRG-1); client cannot set.

## 5.2 Authorization Rules

**AUTH-1..AUTH-8:**
- **AUTH-1** — Tenant-level reads require **active** membership in target tenant.
- **AUTH-2** — Tenant-admin mutations (add/remove/change roles/group mappings/dept activation) require `tenant_admin` OR `tenant_owner`.
- **AUTH-3** — Tender ACL management requires `tender_admin`, `tenant_admin`, or `tenant_owner`.
- **AUTH-4** — Delegation create is self-service; cancelling **another user's** delegation requires `tenant_admin`/`tenant_owner`. Both delegator and delegate must be active members (DEL-1).
- **AUTH-5** — System principal (`iam-system`, `…00a1`) accepted only on `/api/v1/internal/*`.
- **AUTH-6** — Every operator route re-checks `platform_operator` from `rc.Roles` **before any DB access**. DB role stays `org_membership_app`.
- **AUTH-7** — `platform_operator` defended in depth: (1) network isolation (operator ingress only, §10.2); (2) handler re-check (AUTH-6); (3) gateway header hygiene (strips client-supplied `x-tenant-roles`, sources `platform_operator` claim only from operator IdP — the gateway/platform-security team's contract, not O&M's enforcement).
- **AUTH-8** — Privilege reduction (P-7 suspend, P-8 removal, P-28 de-privilege) commits O&M state first (authoritative for O&M's own authz), then makes a **best-effort, fail-open** `RevokeUserSessions` call to Realm Provisioner. Guaranteed cutoff falls back to TTL backstop (≤ access-token lifetime + 300 s `om:memberships` cache). I-5 hard-delete needs no call (Keycloak deletion already kills sessions). Metric: `iam_session_revoke_failed_total` — sustained rate pages.

## 5.3 Endpoint Catalogue

### Public routes (`/api/v1/*`)

| # | Method & Path | Purpose | AuthZ | Cache |
|---|---|---|---|---|
| P-1 | `GET /tenants/:id` | Tenant details (name, plan, locale, mfa_freshness) | same-tenant member | yes |
| P-2 | `PATCH /tenants/:id` | Update name/locale/local_accounts_enabled/mfa_freshness_seconds | tenant_owner | invalidates |
| P-3 | `GET /tenants/:id/departments` | List active depts | member | yes |
| P-4 | `GET /tenants/:id/members` | List members (cursor-paginated, §16 A4) | member | yes (page 1, limit=50 only — CACHE-10) |
| P-5 | `GET /tenants/:id/members/:user_id` | Single member | member | yes |
| P-6 | `POST /tenants/:id/members` | **Invite** (two-step invite→accept, §16 A11); stages `pending_invitations` + RP CreateInvitedUser; returns 202; seat-cap gated (SEAT-1) — `409 seat_limit_reached` at/above cap | tenant_admin/owner | invalidates seat-usage |
| P-7 | `PATCH /tenants/:id/members/:user_id` | Suspend/reactivate (status only; roles → P-28) | tenant_admin/owner | invalidates |
| P-8 | `DELETE /tenants/:id/members/:user_id` | Remove user — **delegate-impact gated** (§8.8); `409 workflow_resolution_required` if delegate on active workflows | tenant_admin/owner | invalidates |
| P-9 | `GET /tenants/:id/departments/:dept_id/members` | Dept members by level | member | yes |
| P-10 | `PUT /tenants/:id/departments/:dept_id/members/:user_id` | Assign to dept at level. **Decrease is dept-scope delegate-impact gated** (§8.8.4) | tenant_admin/owner | invalidates |
| P-11 | `DELETE /tenants/:id/departments/:dept_id/members/:user_id` | Dept remove — dept-scope delegate-impact gated (§8.8.4) | tenant_admin/owner | invalidates |
| P-12 | `GET /tenants/:id/roles` | Role catalog (`dept_role_labels`) | member | yes |
| P-13 | `PATCH /tenants/:id/roles/:role_code` | Update `display_name` only | tenant_admin/owner | invalidates |
| P-14/P-15/P-16/P-17/P-29 | *retired* | — | Group→role/department mapping CRUD moved to Group Mapping Service (`group-mapping-jit-config`, ADR-0007 Wave 2), along with `group_dept_role_mappings`/`group_tenant_role_mappings`/`group_dept_mappings` themselves — see `database-schema.md`. IDs never reused. |
| P-18/P-19/P-20 | `GET`/`POST`/`DELETE /delegations[/:id]` | List/create/cancel delegation (POST also coordinates User Profile) | self / self+admin | see caching |
| P-21/P-22/P-23 | `GET`/`POST`/`DELETE /tenants/:id/tenders/:tender_id/acl[/:user_id]` | List/grant/revoke tender ACL (accepts optional `reason`, `expires_at`) | tender_admin/tenant_admin/tenant_owner | yes/invalidates |
| P-24/P-25 | `POST`/`PATCH /tenants/:id/departments[/:dept_id]` | Activate/deactivate/reactivate tenant department | tenant_admin/owner | invalidates |
| P-26 | `POST /tenants/:id/users/:user_id/removal-resolution` | Resolve blocked removal/demotion — `replace_delegate` or `stop_workflows` (§8.8.3/§8.8.4) | tenant_admin/owner | invalidates |
| P-27 | `GET /tenants/:id/seat-usage` | `{active_users, pending_invitations, licensed_seats, over_cap, overage_since, grace_ends_at}` | tenant_admin/owner | yes (30 s TTL) |
| P-28 | `PUT /tenants/:id/members/:user_id/roles` | Full-replacement multi-role reconcile; `422 last_owner_removal` guard (TM-8) | tenant_admin/owner | invalidates |
| P-30/P-31 | `GET`/`DELETE /tenants/:id/invitations[/:invitation_id]` | List/revoke pending invitations (P-31 sets `kc_cleanup_pending`, PI-6) | tenant_admin/owner | no/invalidates seat-usage |

### Internal routes (`/api/v1/internal/*`)

| # | Method & Path | Caller | Purpose |
|---|---|---|---|
| I-1 | `POST /tenants` | Realm Provisioner / Signup BFF | Provision tenant row |
| I-2 | `PATCH /tenants/:id` | Realm Provisioner | Set `realm_id`, `realm_type='dedicated'`, `keycloak_shard` (all together) after dedicated realm provisioning |
| I-3 | `POST /tenants/:id/members` | Event Consumer | Add membership from Keycloak `REGISTER` — **also invitation-acceptance path** (§8.10, PI-4): flips matching pending row to `accepted`, applies queued roles/depts |
| I-4 | `PATCH /tenants/:id/members/:user_id` | Event Consumer | Update status from Keycloak lifecycle |
| I-5 | `DELETE /tenants/:id/members/:user_id` | Event Consumer | Soft-delete on Keycloak `USER_DELETE` — **delegate-impact gated** (§8.8, WFI-1); sets `ownerless_since` if last owner (TM-12/T-13) |
| I-6/I-7 | *retired (§16 A26)* | — | Quota moved to Usage & Metering |
| **I-8** | `GET /users/:id/memberships` | **AuthZ Enrichment (hot path)** | Full membership context for header injection |
| I-9 | `GET /tenants/:id/locale` | LLM Service | Tenant default locale for prompt assembly |
| I-10 | `POST /tenants/:id/dept-memberships` | Event Consumer | SAML group assertion → dept memberships **+ additive tenant-role grants** (§8.5, GTRM-4) |
| I-11 | `GET /tenants/:id/seat-usage` | Billing | Same handler as P-27; pre-check before seat reduction |
| I-12 | `GET /tenants/:id/tenders/:tender_id/acl/:user_id` | Tender Service / AuthZ | Service-to-service tender-ACL check: `{has_access, access_level}` for active grant (TAE-3) |
| I-13 | `POST /tenants/:id/tenders/:tender_id/assignee-override` | Workflow Service | Validate-and-emit for node reassignment. Body `{new_user_id, department_id, required_level, actor_id}`. Checks actor holds `tender_admin` (`403 insufficient_role`) and new assignee is active member at `required_level` in `department_id` (`422 assignee_ineligible`). Emits `TenderAssigneeOverridden` on pass. **O&M persists nothing** (OVR-1). |

**Internal API invariants (IAPI-1..5):**
- **IAPI-1** — Internal routes callable only by mTLS-authenticated in-mesh services; external ingress blocked.
- **IAPI-2** — System principal (`…00a1`) accepted only on internal routes.
- **IAPI-3** — Internal provisioning executes under target tenant's context; still subject to RLS `WITH CHECK` (RLS-5).
- **IAPI-5** — I-8 is the authoritative membership projection; changes are versioned, not breaking.

### Operator routes (`/api/v1/operator/*`)

**O-1/O-2/O-3 (department CRUD) and O-5/O-6 (plan read/edit) moved to the standalone Catalog / Admin Config Service** (`iam-catalog-admin`, migration-runbook Phase 4) along with the `departments`/`plans` tables themselves — see `database-schema.md`. Only O-4 and O-7 remain in O&M's `/operator` group:

| # | Method & Path | Purpose |
|---|---|---|
| O-4 | `PATCH /tenants/:id/feature-flags` | Full-replacement of override delta (§16 A18). Allow-list validated; scalars only |
| O-7 | `POST /tenants/:id/reassign-owner` | Recover ownerless tenant — grant `tenant_owner` to existing active member; clear `ownerless_since` (§16 A39, TM-12/T-13). Only path to resolve TM-12 escalation |

**Operator invariants (OP-1..7):** Only `platform_operator` writes feature_flags/reassign-owner in O&M (catalog/plans writes now live in the Catalog Service). Departments never physically deleted (OP-3, enforced by the Catalog Service now). Retirement affects future assignments only, existing rows untouched (OP-5).

## 5.5 Status Codes

Per `gincommon.ErrorResponse` `{code, message, request_id, trace_id, details}`. Service-specific triggers noted:

| Code | Meaning | Triggers |
|---|---|---|
| 200 / 201 / 202 / 204 | Success | 202 = P-6 invite staged |
| 400 | Validation | `invalid_uuid`, `invalid_locale`, `invalid_slug`, `invalid_role_level`, `invalid_role` (incl. `member`, TR-7), `invalid_delegation_scope`, `invalid_access_level` (view/edit/approve, §16 A32(c)), `invalid_limit`, `invalid_cursor`, `unknown_feature_flag` (O-4 allow-list), `invalid_feature_value` (non-scalar, PLAN-6(d)), `invalid_mfa_freshness_seconds` (T-10) |
| 401 | `missing_identity_headers` |
| 403 | `insufficient_role`, `cannot_remove_owner` |
| 404 | `tenant_not_found`, `member_not_found`, `department_not_found`, `delegation_not_found`, `invitation_not_found` (revoke on non-pending) |
| 409 | Conflict/optimistic-lock/race | `slug_already_taken`, `member_already_exists`, `dept_membership_already_exists`, `optimistic_lock_conflict` (echoes current `record_version`), **`workflow_resolution_required`** (§8.8/§8.8.4 — body has `active_workflows`, `workflow_ids`, `allowed_actions: [replace_delegate, stop_workflows]`, WFI-3), **`seat_limit_reached`** (SEAT-1 — body has `licensed_seats`, `active_users`, `pending_invitations`), `invitation_already_exists` (PI-1), `tenant_offboarded` (O-7 on terminal) |
| 422 | Domain rule | `self_delegation`, `invalid_delegate` (DEL-1 or User Profile 4xx race), `delegation_window_inverted`, `scope_id_required`, `cannot_delete_system_department`, `department_not_active_for_tenant`, `invalid_replacement` (§8.8 WFI-5), `invalid_owner_candidate` (O-7 not active member), `invalid_expires_at`, `assignee_ineligible` (I-13 — `422`, not `409`, per §16 A62), `field_immutable`, `system_name_immutable`, `system_department_cannot_be_retired`, `last_owner_removal` (TM-8 — actor path P-8/P-28) |
| 429 | Invite abuse only (§16 A41) | `reinvite_too_soon` (PI-11, `INVITE_REINVITE_COOLDOWN_MINUTES`), `invite_rate_limited` (PI-12, `INVITE_MAX_PER_TENANT_PER_HOUR`). Body includes `retry_after_seconds`. **Quota/API-rate 429 remains gateway + Usage & Metering** (HLD §10.6) |
| 503 | Dependency | `db_unavailable`, `cache_unavailable` (degraded — cache advisory), `user_profile_unavailable` (delegation), `workflow_service_unavailable` (delegate-impact/resolution, WFI-8), `realm_provisioner_unavailable` (P-6 invite, §8.10) |

## 6. Caching

AWS ElastiCache Valkey via `go-redis/v9`. **Advisory only** — Postgres is source of truth; miss/timeout falls through (CACHE-2/CACHE-9). 50 ms operation timeout = miss. All keys tenant-prefixed (CACHE-1). Invalidation post-commit `DEL` (CACHE-6).

### 6.1 Keys

| Key | Value | TTL | Invalidated by |
|---|---|---|---|
| `om:memberships:{tenant}:{user}` | JSON of full I-8 response | 300 s ±30 s jitter (CACHE-4) | any membership/role/dept write for this user; §8.9 user-deletion cascade |
| `om:tenant:{tenant}` | JSON tenant (plan, locale, flags) | 600 s | `PATCH /tenants/:id` (P-2); Realm Provisioner I-2 |
| `om:members:{tenant}:{limit}` | P-4 page-1 only (cursorless), `limit=50` only (CACHE-10) | 120 s | any membership add/remove/update for tenant (CACHE-7) |
| `om:dept_members:{tenant}:{dept}` | JSON dept member list | 120 s | any dept_memberships write for tenant+dept |
| `om:locale:{tenant}` | BCP-47 string | 600 s | P-2 with locale change |
| `om:roles:{tenant}` | Role catalog | 600 s | P-13 |
| `om:grm:{tenant}` | Group→dept-role mappings, read through `GroupMappingClient` from Group Mapping Service's GM-I1 (ADR-0007 Wave 2 — `group_dept_role_mappings` table dropped from O&M) | 600 s | Passive expiry only — O&M no longer writes these tables, so nothing evicts this key on write; a stale read can live up to the TTL |
| `om:grm:stale:{tenant}` | Same payload as `om:grm:{tenant}`, second-tier stale-if-error fallback — served when Group Mapping Service is unreachable and the primary key has expired | 24 h | Refreshed on every successful primary-key populate; never explicitly evicted |
| `om:gdm:{tenant}` | Group→department mappings, same read-through/GM-I1 sourcing as `om:grm` | 600 s | Passive expiry only, same rationale as `om:grm` |
| `om:gdm:stale:{tenant}` | Same payload as `om:gdm:{tenant}`, second-tier stale-if-error fallback | 24 h | Refreshed on every successful primary-key populate; never explicitly evicted |
| `om:gtrm:{tenant}` | Group→tenant-role mappings, same read-through/GM-I1 sourcing as `om:grm` — closes a pre-existing gap (this dimension previously had no cache at all) | 600 s | Passive expiry only, same rationale as `om:grm` |
| `om:gtrm:stale:{tenant}` | Same payload as `om:gtrm:{tenant}`, second-tier stale-if-error fallback | 24 h | Refreshed on every successful primary-key populate; never explicitly evicted |
| `om:seat_usage:{tenant}` | `{active, pending, licensed_seats, over_cap, overage_since, grace_ends_at}` | 30 s (CACHE-5) | membership add/remove; invite create/revoke/expire/accept; `TenantSeatsChanged`; overage set/clear |
| `om:plans` | Whole plan catalog, read through `CatalogService`/`CatalogAdminClient` from the Catalog Service (Phase 2 read-cutover; `plans` table dropped from O&M in Phase 4) | 600 s | Passive expiry only — O&M no longer writes plans, so nothing evicts this key on write; a stale read can live up to the TTL |
| `om:plans:stale` | Same payload as `om:plans`, second-tier stale-if-error fallback (CAT-D4) — served when the Catalog Service is unreachable and the primary key has expired | 24 h | Refreshed on every successful primary-key populate; never explicitly evicted |
| `om:departments` | Whole department catalog, read through `CatalogService`/`CatalogAdminClient` from the Catalog Service (Phase 2 read-cutover; `departments` table dropped from O&M in Phase 4) | 600 s | Passive expiry only, same rationale as `om:plans` |
| `om:departments:stale` | Same payload as `om:departments`, second-tier stale-if-error fallback (CAT-D4) | 24 h | Refreshed on every successful primary-key populate; never explicitly evicted |

### 6.2 I-8 Hot-Path Query

Single joined query over `tenant_memberships` + `tenants` + `tenant_roles` + `dept_memberships` + `delegations`, filtered `WHERE tm.tenant_id=$1 AND tm.user_id=$2 AND tm.deleted_at IS NULL`. Uses `array_agg(...) FILTER (WHERE ... IS NOT NULL)` + `COALESCE(..., '{}')` so `departments`/`active_delegations` are **always arrays, never null** (I8-4). Derived `member` role injected via set-union at projection layer (`resp.Roles = union(["member"], resp.Roles)` — TR-7/§16 A29). `DISTINCT` in `tenant_roles`' `array_agg` is defensive against future join-restructuring.

**I8 invariants (I8-1..5):**
- **I8-1** — Authoritative membership projection for AuthZ Enrichment.
- **I8-2** — Cache miss/timeout/outage always resolves from Postgres (CACHE-2).
- **I8-3** — Only active membership returns a result; else `404` (AuthZ treats as "no context → deny").
- **I8-4** — `departments` / `active_delegations` normalized to `[]`.
- **I8-5** — Successful lookup cached 300 s ± 30 s; invalidated on any membership/role/dept write for that user.

### 6.5 Cache Invariants

- **CACHE-1** — All keys tenant-scoped (mirrors RLS boundary).
- **CACHE-2** — Cache is advisory; Postgres is source of truth.
- **CACHE-3** — Mutations invalidate synchronously (post-commit `DEL`); TTL provides self-healing.
- **CACHE-4** — Membership entries use TTL jitter to prevent stampede.
- **CACHE-5** — Seat-usage short TTL (30 s); SEAT-1 always re-reads Postgres under `FOR UPDATE`.
- **CACHE-6** — Post-commit invalidation only.
- **CACHE-7** — Membership mutations evict **both** per-user (`om:memberships`) and list (`om:members`) projections.
- **CACHE-8** — User-deletion cascade evicts `om:memberships:{tenant}:{user}` for every tenant the user was in (read set from `tenant_memberships` before soft-delete).
- **CACHE-9** — Cache is a performance, not correctness, dependency. Miss/timeout/outage = degraded latency only. `/readyz` reports degraded, stays ready while Postgres is healthy.
- **CACHE-10** — Only P-4 cursorless first page cached, only at default `limit=50`. Other limits/pages bypass cache (avoids `SCAN` for enumerating limit values on invalidation).

## 7. Event Architecture

### 7.1 Inbound (SQS)

**`tenant-orgm-q` ← `iam.tenant.events`** (produced by Realm Provisioner): `TrialTenantProvisioned`, `TenantRealmReady` (sets `realm_id`/`realm_type='dedicated'`/`keycloak_shard` together), `TenantConverted` (sets `status='active'`, `subscription_started_at=now()`, `plan`; **`feature_flags` untouched** — override survives plan change, T-9), `DirectPaidSignup`, `TrialExpired` (sets `status='trial_expired'`; **no PII scrub yet** — reactivatable during 15-d grace), `TrialReactivated`, `TenantSuspended`, `TenantOffboarded`.

**`billing-orgm-q` ← `billing.events`** (produced by Billing): `TenantPlanChanged` (`feature_flags` untouched, T-9), `TenantPaymentPastDue` (sets `status='past_due'`; **access unchanged** per HLD §8.10.7), `TenantSubscriptionCancelled` (sets `status='cancelled'` AND `cancelled_at=now()` together, T-11), `TenantReactivated` (sets `status='active'` AND `cancelled_at=NULL` together; only valid pre-offboard), `TenantSeatsChanged` (unconditional `licensed_seats` update, SEAT-2).

Both queues: DLQ with `maxReceiveCount=5`, `processed_events` dedup, PgBouncer-safe RLS binding.

**EVT-14 recency guard (§16 A33, tenants projection last-writer-wins):** Every handler compares `event.time` vs `tenants.last_event_at` **under the tenant row lock**. If `event.time <= last_event_at`, skip state change but still record `processed_events` (event stale/reordered — `iam_stale_lifecycle_event_skipped_total`++). Otherwise apply + set `last_event_at = event.time` in the same `UPDATE`. Makes projection commutative under reordering. `last_event_at` **never advanced by API writes** — only consumed events. Tie (`==`) treated as stale.

**EVT-15 future-time clamp (§16 A40, poison-pill guard):** If `event.time > now() + MAX_LIFECYCLE_EVENT_SKEW_SECONDS` (default 300 s), event is **rejected to DLQ** (not applied, `last_event_at` not advanced, **not** recorded in `processed_events`). `iam_future_lifecycle_event_rejected_total`++ — any nonzero pages (a producer's clock is skewed).

**EVT-16 tenant-state relay (§16 A61):** Whenever a consumed handler **actually changes `tenants.status` or `tenants.plan`** (post-EVT-14 check), enqueue a **`TenantStateChanged`** event on `iam.membership.events` **in the same `RunInTx`** as the projection UPDATE. Carries `{status, previous_status, plan, previous_plan, changed_at, cause}`. Lets Workflow Service pause/resume/terminate/route without a direct `iam.tenant.events`/`billing.events` subscription. Never fires on stale-skip or no-op.

### 7.3 Outbound (SNS, via `RoutingPublisher`)

**`iam.membership.events`:**

| Event | Trigger | Key payload |
|---|---|---|
| `DepartmentMembershipGranted` | User added to dept | `user_id`, `tenant_id`, `department_id`, `level`, `actor_id` |
| `DepartmentMembershipRevoked` | User removed from dept | `user_id`, `tenant_id`, `department_id`, `actor_id` |
| `DepartmentMembershipLevelChanged` | Level changed | `user_id`, `tenant_id`, `department_id`, `previous_level`, `new_level`, `actor_id` |
| `TenantRoleGranted` | Elevated tenant role granted (init, P-28, or JIT GTRM-4). **One event per role_code**, not bulk | `user_id`, `tenant_id`, `role_code`, `actor_id` |
| `TenantRoleRevoked` (§16 A14) | Elevated tenant role revoked (P-28 or removal cascade TR-9). One event per revoked role | `user_id`, `tenant_id`, `role_code`, `actor_id` |
| `DelegationStarted` | Delegation created | `delegation_id`, `tenant_id`, `delegator_id`, `delegate_id`, `scope`, `scope_id`, `ends_at`, `actor_id` |
| `DelegationEnded` | Expired, cancelled, **or delegate removed** (DEL-7) | above + `ended_reason ∈ {expired, cancelled, delegate_removed}` |
| `TenderAssigneeOverridden` | I-13 validate-and-emit — Workflow Service call | `tender_id`, `tenant_id`, `user_id`, `actor_id` |
| `TenantSeatOverageStarted` | `overage_since` NULL→set (SEAT-5) | `tenant_id`, `licensed_seats`, `active_users`, `pending_invitations`, `overage_since` |
| `TenantSeatOverageResolved` | `overage_since` set→NULL | `tenant_id`, `resolved_at` |
| `TenantStateChanged` (§16 A61) | Post-EVT-14 status/plan change | `tenant_id`, `status`, `previous_status`, `plan`, `previous_plan`, `changed_at`, `cause` |

**`iam.tenant.events`:**

| Event | Trigger | Key payload |
|---|---|---|
| `TenantCreated` | New tenant row | `tenant_id`, `slug`, `plan`, `status` |
| `TrialStarted` | Trial signup | `tenant_id`, `plan`, `trial_ends_at` |

O&M publishes **only these two** on `iam.tenant.events`. Lifecycle events O&M consumes on this topic are all Realm-Provisioner-produced.

### 7.3.2 SNS→SQS Fan-out (§16 A60)

`iam.membership.events` consumers: `membership-audit-q` (Audit — no filter, catch-all); `membership-authz-q` (AuthZ — dept/tenant role events for cache eviction); `membership-realm-q` (RP — approver make/unmake + admin/owner for `requires-mfa` realm role); `membership-notification-q` (Notification — user/admin emails + seat-overage banner); `membership-workflow-q` (Workflow — delegation/override/`TenantStateChanged`); `membership-billing-q` (Billing — `TenantSeatOverage*` only, filter policy).

`iam.tenant.events` consumers: `tenant-audit-q`, `tenant-notification-q` (welcome/trial-start emails). O&M's own `tenant-orgm-q` is separate — produce/consume disjoint.

Every subscribing queue: `-dlq`, `maxReceiveCount=5`, `processed_events` dedup.

### 7.4 CloudEvents Envelope

`{id (UUID v7), source, tenant_id, trace_id, specversion, time, subject, actor, dataschema, data}`. Governed by `platform-schemagov` — `api/asyncapi.yaml` is design-time source of truth; `internal/eventschema/*.json` derived via `schema-gov extract`; Glue Schema Registry runtime enforcement (ap-south-1). CI: `docker run ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:v0.3.0` for extract → validate → enforce-lifecycle → diff (PR) / register (main).

### 7.5 Event Invariants

- **EVT-4** — `processed_events` PK `(event_id, consumer)` = canonical dedup.
- **EVT-5** — `maxReceiveCount=5` → DLQ; never silent drop.
- **EVT-6** — Illegal state transitions rejected (e.g. any move off `offboarded`, PAID-1).
- **EVT-10** — Transactional outbox: business write + event share one `RunInTx`.
- **EVT-11** — Invitation lifecycle is audit-only; acceptance rides existing `TenantRoleGranted`/`DepartmentMembershipGranted` (PI-7).
- **EVT-14** — Recency guard on `tenants` projection (above).
- **EVT-15** — Future-time clamp (above).
- **EVT-16** — Tenant-state relay in same tx as projection UPDATE (above).
