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
- **AUTH-3** — *Retired* — tender-ACL management authz moved to the Tender ACL Service (`iam-tender-acl`, ADR-0007 Wave 3) along with the ACL endpoints themselves (P-21/22/23, I-12).
- **AUTH-4** — *Retired* — delegation create/cancel authz moved to the standalone Delegation Service (`iam-delegation`, ADR-0008) along with the delegation endpoints themselves (P-18/19/20/32/33).
- **AUTH-5** — System principal (`iam-system`, `…00a1`) accepted only on `/api/v1/internal/*`.
- **AUTH-6** — Every operator route re-checks `platform_operator` from `rc.Roles` **before any DB access**. DB role stays `org_membership_app`.
- **AUTH-7** — `platform_operator` defended in depth: (1) network isolation (operator ingress only, §10.2); (2) handler re-check (AUTH-6); (3) gateway header hygiene (strips client-supplied `x-tenant-roles`, sources `platform_operator` claim only from operator IdP — the gateway/platform-security team's contract, not O&M's enforcement).
- **AUTH-8** — Privilege reduction (P-7 suspend, P-8 removal, P-28 de-privilege) commits O&M state first (authoritative for O&M's own authz), then makes a **best-effort, fail-open** `RevokeUserSessions` call to Realm Provisioner. Guaranteed cutoff falls back to TTL backstop (≤ access-token lifetime + 300 s `om:memberships` cache). I-5 hard-delete needs no call (Keycloak deletion already kills sessions). Metric: `iam_auth_session_revoke_failed_total` — sustained rate pages.
- **AUTH-9** (new, IB-3) — Service-account subjects are non-grantable, defense-in-depth on top of the structural composite-FK bar (`tenant_roles`/`dept_memberships` → `tenant_memberships`, TR-8/DM-4). Membership-create (P-6/I-3) and role-grant (P-10/P-28) reject a target that `port.TokenServiceClient.IsServiceAccount` (`GET {TOKEN_SERVICE_BASE_URL}/service-accounts?principal_sub=`, Token Service's TS-5) reports as a `service_account`-typed Keycloak principal, before any other check — `403 service_account_not_grantable`. **Fails open**: an empty/unreachable Token Service degrades to allowing the operation, since the FK bar remains the primary guarantee.

## 5.3 Endpoint Catalogue

### Public routes (`/api/v1/*`)

| # | Method & Path | Purpose | AuthZ | Cache |
|---|---|---|---|---|
| P-1 | `GET /tenants/:id` | Tenant details (name, plan, locale, mfa_freshness) | same-tenant member | yes |
| P-2 | `PATCH /tenants/:id` | Update name/locale/local_accounts_enabled/mfa_freshness_seconds | tenant_owner | invalidates |
| P-3 | `GET /tenants/:id/departments` | List active depts | member | yes |
| P-4 | `GET /tenants/:id/members` | List members (cursor-paginated, §16 A4) | member | yes (page 1, limit=50 only — CACHE-10) |
| P-5 | `GET /tenants/:id/members/:user_id` | Single member | member | yes |
| P-6 | `POST /tenants/:id/members` | **Invite** (two-step invite→accept, §16 A11); stages `pending_invitations` + RP CreateInvitedUser (`required_actions` computed from `initial_tenant_roles`/`initial_dept_mappings` — `CONFIGURE_TOTP` added for tenant_admin/owner or Approver-level grants, F5 resolved); returns 202; seat-cap gated (SEAT-1) — `409 seat_limit_reached` at/above cap | tenant_admin/owner | invalidates seat-usage |
| P-7 | `PATCH /tenants/:id/members/:user_id` | Suspend/reactivate (status only; roles → P-28) | tenant_admin/owner | invalidates |
| P-8 | `DELETE /tenants/:id/members/:user_id` | Remove user — **delegate-impact gated** (§8.8); `409 workflow_resolution_required` if delegate on active workflows | tenant_admin/owner | invalidates |
| P-9 | `GET /tenants/:id/departments/:dept_id/members` | Dept members by level | member | yes |
| P-10 | `PUT /tenants/:id/departments/:dept_id/members/:user_id` | Assign to dept at level. **Decrease is dept-scope delegate-impact gated** (§8.8.4) | tenant_admin/owner | invalidates |
| P-11 | `DELETE /tenants/:id/departments/:dept_id/members/:user_id` | Dept remove — dept-scope delegate-impact gated (§8.8.4) | tenant_admin/owner | invalidates |
| P-12 | `GET /tenants/:id/roles` | Role catalog (`dept_role_labels`) | member | yes |
| P-13 | `PATCH /tenants/:id/roles/:role_code` | Update `display_name` only | tenant_admin/owner | invalidates |
| P-14/P-15/P-16/P-17/P-29 | *retired* | — | Group→role/department mapping CRUD moved to Group Mapping Service (`group-mapping-jit-config`, ADR-0007 Wave 2), along with `group_dept_role_mappings`/`group_tenant_role_mappings`/`group_dept_mappings` themselves — see `database-schema.md`. IDs never reused. |
| P-18/P-19/P-20/P-32/P-33 | *retired* | — | List/create/cancel/extend/reassign delegation moved to the standalone Delegation Service (`iam-delegation`, ADR-0008) — DLG-1..5, along with the `delegations` table itself and `tenants.delegation_max_duration_days`/`delegation_review_window_days` — see `database-schema.md`. IDs never reused. |
| P-21/P-22/P-23 | *retired* | — | List/grant/revoke tender ACL moved to the Tender ACL Service (`iam-tender-acl`, ADR-0007 Wave 3) — TAC-1/2/3, along with `tender_acl_entries` (+ `tender_acl_level` ENUM) — see `database-schema.md`. IDs never reused. |
| P-24/P-25 | `POST`/`PATCH /tenants/:id/departments[/:dept_id]` | Activate/deactivate/reactivate tenant department | tenant_admin/owner | invalidates |
| P-26 | `POST /tenants/:id/users/:user_id/removal-resolution` | Resolve blocked removal/demotion — `replace_delegate` or `stop_workflows` (§8.8.3/§8.8.4) | tenant_admin/owner | invalidates |
| P-27 | `GET /tenants/:id/seat-usage` | `{active_users, pending_invitations, licensed_seats, over_cap, overage_since, grace_ends_at}` | tenant_admin/owner | yes (30 s TTL) |
| P-28 | `PUT /tenants/:id/members/:user_id/roles` | Full-replacement multi-role reconcile; `422 last_owner_removal` guard (TM-8) | tenant_admin/owner | invalidates |
| P-30/P-31 | `GET`/`DELETE /tenants/:id/invitations[/:invitation_id]` | List/revoke pending invitations (P-31 sets `kc_cleanup_pending`, PI-6) | tenant_admin/owner | no/invalidates seat-usage |
| P-34 | `POST /tenants/:id/members/:user_id/reset-mfa` | **NEW (§16 OQ-8/F6)** — reset a member's MFA via Realm Provisioner (RP-9); `422 member_not_active`, `503 realm_provisioner_unavailable` (fail-closed, no reconciler). Emits `MFAReset` | tenant_admin/owner | no |

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
| I-12 | *retired* | — | Tender-ACL check moved to the Tender ACL Service (`iam-tender-acl`, ADR-0007 Wave 3) — TAC-4. ID never reused. |
| I-13 | `POST /tenants/:id/tenders/:tender_id/assignee-override` | Workflow Service | Validate-and-emit for node reassignment. Body `{new_user_id, department_id, required_level, actor_id}`. Checks actor holds `tender_admin` (`403 insufficient_role`) and new assignee is active member at `required_level` in `department_id` (`422 assignee_ineligible`). Emits `TenderAssigneeOverridden` on pass. **O&M persists nothing** (OVR-1). |
| I-14 | `GET /tenants/:id/mfa-freshness` | AuthZ Enrichment | Returns `mfa_freshness_seconds` from the `om:tenant` cache — authoritative read path for the Approver step-up gate (§16 A72); modeled on I-9's same cache-through shape |
| I-15 | `GET /tenants/:id/members/:user_id/exists` | Tender ACL Service, Delegation Service | **NEW** — grant-time membership-existence check, replacing the composite membership FKs both services lost when their tables moved to separate databases; response `{active, tenant_membership_id}` (`tenant_membership_id` populated only when active); never 404s |
| I-16 | `GET /subscription-lapses` | Realm Provisioner (RP-C3 lapse sweep) | **NEW (§16 OQ-9)** — cross-tenant bulk read: every `status='cancelled'` tenant past `SUBSCRIPTION_GRACE_DAYS`; `{tenants: [{tenant_id, realm_id, realm_type, cancelled_at}]}`. RP polls this instead of tracking `cancelled_at` itself (deliberately pull, not push — see RP-4's opposite resolution for trial-expiry). Only internal route not scoped under `/tenants/:id`; served against the BYPASSRLS `sysPool`, never the RLS-scoped app pool. Self-idempotent — RP suspending a tenant removes it from the next poll |

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
| 400 | Validation | `invalid_uuid`, `invalid_locale`, `invalid_slug`, `invalid_role_level`, `invalid_role` (incl. `member`, TR-7), `invalid_limit`, `invalid_cursor`, `unknown_feature_flag` (O-4 allow-list), `invalid_feature_value` (non-scalar, PLAN-6(d)), `invalid_mfa_freshness_seconds` (T-10) |
| 401 | `missing_identity_headers` |
| 403 | `insufficient_role`, `cannot_remove_owner`, `service_account_not_grantable` (AUTH-9 — P-6/I-3 invite/register, P-10/P-28 role-grant target resolves to a service-account principal) |
| 404 | `tenant_not_found`, `member_not_found`, `department_not_found`, `invitation_not_found` (revoke on non-pending) |
| 409 | Conflict/optimistic-lock/race | `slug_already_taken`, `member_already_exists`, `dept_membership_already_exists`, `optimistic_lock_conflict` (echoes current `record_version`), **`workflow_resolution_required`** (§8.8/§8.8.4 — body has `active_workflows`, `workflow_ids`, `allowed_actions: [replace_delegate, stop_workflows]`, WFI-3), **`seat_limit_reached`** (SEAT-1 — body has `licensed_seats`, `active_users`, `pending_invitations`), `invitation_already_exists` (PI-1), `tenant_offboarded` (O-7 on terminal) |
| 422 | Domain rule | `cannot_delete_system_department`, `department_not_active_for_tenant`, `invalid_replacement` (§8.8 WFI-5), `invalid_owner_candidate` (O-7 not active member), `assignee_ineligible` (I-13 — `422`, not `409`, per §16 A62), `field_immutable`, `system_name_immutable`, `system_department_cannot_be_retired`, `last_owner_removal` (TM-8 — actor path P-8/P-28) |
| 429 | Invite abuse only (§16 A41) | `reinvite_too_soon` (PI-11, `INVITE_REINVITE_COOLDOWN_MINUTES`), `invite_rate_limited` (PI-12, `INVITE_MAX_PER_TENANT_PER_HOUR`). Body includes `retry_after_seconds`. **Quota/API-rate 429 remains gateway + Usage & Metering** (HLD §10.6) |
| 503 | Dependency | `db_unavailable`, `cache_unavailable` (degraded — cache advisory), `workflow_service_unavailable` (delegate-impact/resolution, WFI-8), `realm_provisioner_unavailable` (P-6 invite, §8.10; also P-34 reset-mfa, §16 OQ-8/F6 — fail-closed, no reconciler), `catalog_unavailable` (Catalog Service departments/plans read-through — `om:plans`/`om:departments` cache empty and the live call also failed; not fail-open), `group_mapping_unavailable` (declared per LLD §17, but `GroupMappingService.resolveMappings` deliberately fails **open** on a cold-cache-plus-live-call-failure per ADR-0007 Action Item 4 — returns an empty resolution, HTTP 200 — so this code is declared but not currently returned by I-10 in practice) |

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

`AuthZService.GetMembership` (`internal/core/service/authz_service.go`) no longer touches Postgres directly — it depends only on `port.AuthZRepository`, never `*pgcommon.Pool` (a Clean-Architecture fix landed alongside I-16). The join itself lives in `internal/adapter/outbound/postgres/authz_repository.go`'s `AuthZRepository.FindMembershipProjection`, which runs **one transaction over the same four tables** (`tenant_memberships` ⋈ `tenants` — one fewer table than pre-decomposition, `delegations` dropped, response no longer carries `active_delegations[]`) but as **three scoped queries, not a single `array_agg`/`JOIN` statement**: (1) `tenant_memberships JOIN tenants` for status/plan/locale/mfa/feature-flags, (2) `tenant_roles` filtered to the same `(tenant_id, user_id)`, (3) `dept_memberships` filtered the same way — all `WHERE tenant_id=$1 AND user_id=$2 AND deleted_at IS NULL`. `AuthZService.readFromDB` then composes the response: TR-7's derived `member` role is prepended to the raw `tenant_roles` set (`append([]domain.TenantRoleCode{domain.RoleMember}, row.Roles...)`), department codes are filled in from the `om:departments` catalog cache, and PLAN-6's effective feature-flag set is merged in. `departments` stays **always an array, never null** (I8-4) because the repository initializes it as an empty (non-nil) slice literal, not via SQL `COALESCE` — there is no `array_agg`/`FILTER`/`DISTINCT` in this query at all.

**I8 invariants (I8-1..5):**
- **I8-1** — Authoritative membership projection for AuthZ Enrichment.
- **I8-2** — Cache miss/timeout/outage always resolves from Postgres (CACHE-2).
- **I8-3** — Only active membership returns a result; else `404` (AuthZ treats as "no context → deny").
- **I8-4** — `departments` normalized to `[]`.
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

**`tenant-orgm-q` ← `iam.tenant.events`** (produced by Realm Provisioner): `TrialTenantProvisioned`, `TenantRealmReady` (sets `realm_id`/`realm_type='dedicated'`/`keycloak_shard` together — the payload itself only carries `realm`/`keycloak_shard`, per RP's frozen `TenantRealmReadyPayload`; `realm_type` is hardcoded `'dedicated'` on this handler's side rather than read from a payload field RP never sends, since this event is only ever emitted by RP-2/RP-3, never for a trial. A prior version of the consumer read nonexistent `realm_id`/`realm_type` fields and silently blanked both columns on every real event — fixed), `TenantConverted` (sets `status='active'`, `subscription_started_at=now()`, `plan`; **`feature_flags` untouched** — override survives plan change, T-9), `DirectPaidSignup`, `TrialExpired` (sets `status='trial_expired'`; **no PII scrub yet** — reactivatable during 15-d grace), `TrialReactivated`, `TenantSuspended` (payload `{source}` selects `LifecycleSuspendBillingLapse` vs `LifecycleSuspendOperator`, T-16; defaults to billing_lapse if ever absent, defensive only — RP confirmed the field is always present), `TenantOffboarded`, `TenantReactivated{source=operator}` (new, resolves F1 of the RP↔O&M alignment review — RP-10 reversing RP-14; same handler case as `billing-orgm-q`'s `TenantReactivated`, T-16).

**`billing-orgm-q` ← `billing.events`** (produced by Billing): `TenantPlanChanged` (`feature_flags` untouched, T-9), `TenantPaymentPastDue` (sets `status='past_due'`; **access unchanged** per HLD §8.10.7), `TenantSubscriptionCancelled` (sets `status='cancelled'` AND `cancelled_at=now()` together, T-11), `TenantReactivated` (rejected as a no-op if already `offboarded`, PAID-1; otherwise clears `cancelled_at` AND `suspension_source` together and resolves the **target status by two conditioned `UPDATE`s, not a hardcoded `'active'`**: `LifecycleReactivatePaid` (`WHERE subscription_started_at IS NOT NULL` → `status='active'`) tried first, falling back to `LifecycleReactivateTrial` (`WHERE subscription_started_at IS NULL` → `status='trial'`) — an operator-suspended tenant that never converted must reactivate to `trial`, not `active`, or it would violate `chk_subscription_started_required`; only valid pre-offboard), `TenantSeatsChanged` (unconditional `licensed_seats` update, SEAT-2).

**`catalog-orgm-q` ← `DepartmentCatalogChanged`** (produced by Catalog / Admin Config Service — Gap 12, `gap_doc/pending_gap_sep15.md`): clears `om:departments`/`om:departments:stale` on receipt, closing the ~11-minute cache-propagation gap on department create/rename/retire. This service's consumer side (`catalog_consumer.go`) is done and now unit-tested, but the queue is **inert in every environment today** — `iam-catalog-admin` has no publisher side yet (no `port.EventNotifier`, no `notify()` call sites, no Terraform), and `SQS_CATALOG_ORGM_QUEUE_URL` is unset everywhere. Now declared in `api/asyncapi.yaml` too (added 2026-09-20 — was previously missing from the design-time source of truth despite the consumer code existing): `sqsCatalogOrgm` server, `catalogOrgmQueue` channel, `consumeCatalogOrgm` operation. Its payload schema is a deliberate open placeholder (`additionalProperties: true`, no fields) since neither this consumer nor iam-catalog-admin's nonexistent producer defines real fields yet.

All three queues: DLQ with `maxReceiveCount=5`, `processed_events` dedup, PgBouncer-safe RLS binding.

**EVT-14 recency guard (§16 A33, tenants projection last-writer-wins):** Every handler compares `event.time` vs `tenants.last_event_at` **under the tenant row lock**. If `event.time <= last_event_at`, skip state change but still record `processed_events` (event stale/reordered — `iam_lifecycle_event_skipped_total`++). Otherwise apply + set `last_event_at = event.time` in the same `UPDATE`. Makes projection commutative under reordering. `last_event_at` **never advanced by API writes** — only consumed events. Tie (`==`) treated as stale.

**EVT-15 future-time clamp (§16 A40, poison-pill guard):** If `event.time > now() + MAX_LIFECYCLE_EVENT_SKEW_SECONDS` (default 300 s), event is **rejected to DLQ** (not applied, `last_event_at` not advanced, **not** recorded in `processed_events`). `platform_dlq_messages_total`++ — any nonzero pages (a producer's clock is skewed).

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
| `MembershipRevoked` (§15.2.2) | User removed from tenant — emitted unconditionally by `MembershipService.RemoveUser` (P-8/I-5) and `ProvisioningService.DeleteMember`'s underlying path. **Consolidated in this pass**: a second, separate `TenantMembershipRemoved` event previously existed for the Tender-ACL Service alone — that's gone. This one shared event is now consumed by **both** the Delegation Service's cascade queue (ends the departed user's delegation rows) and the Tender-ACL Service's cascade queue (soft-deletes the departed user's ACL overlays) | `tenant_id`, `user_id`, `actor_id` |
| `TenderAssigneeOverridden` | I-13 validate-and-emit — Workflow Service call | `tender_id`, `tenant_id`, `user_id`, `actor_id` |
| `MFAReset` (§16 OQ-8/F6) | P-34 — emitted after `RealmProvisionerClient.ResetMFA` (RP-9) succeeds; sole audit record for the reset (O&M persists no MFA state itself); consumed only by `membership-audit-q`'s catch-all filter | `tenant_id`, `user_id`, `actor_id` |
| `TenantSeatOverageStarted` | `overage_since` NULL→set (SEAT-5) | `tenant_id`, `licensed_seats`, `active_users`, `pending_invitations`, `overage_since` |
| `TenantSeatOverageResolved` | `overage_since` set→NULL | `tenant_id`, `resolved_at` |
| `TenantStateChanged` (§16 A61) | Post-EVT-14 status/plan change | `tenant_id`, `status`, `previous_status`, `plan`, `previous_plan`, `changed_at`, `cause` |
| `TenantMembershipsPurged` | Tenant genuinely transitions to `offboarded` (i.e. RP's consumed `TenantOffboarded` actually changes `tenants.status`, post-EVT-14) — emitted by `membership_event_consumer.go` so the Delegation, Tender-ACL, and Group-Mapping services can run their own tenant-scoped cascade-deletes (their rows live in separate databases, out of reach of O&M's own `ON DELETE CASCADE`). **Renamed in this pass** from a prior signal that was (incorrectly) also called `TenantOffboarded` and routed on `iam.tenant.events` — that violated "one producer per event name," since the real `TenantOffboarded` is RP's own terminal event, which O&M only *consumes* (unchanged — still `tenant-orgm-q`/`iam.tenant.events`, §7.1). Only O&M's own outbound relay was renamed and moved topics | `tenant_id`, `actor_id` |

**Removed from this topic in this pass** (moved to the standalone Delegation Service's own topic `iam.delegation.events`): `DelegationStarted`, `DelegationEnded`, `DelegationReviewRequested`. Their JSON schemas are deleted from `internal/adapter/outbound/eventbus/schemas/`.

**`iam.tenant.events`:**

| Event | Trigger | Key payload |
|---|---|---|
| `TenantCreated` | New tenant row | `tenant_id`, `slug`, `plan`, `status` |
| `TrialStarted` | Trial signup | `tenant_id`, `plan`, `trial_ends_at` |

O&M publishes **only these two** on `iam.tenant.events`. Lifecycle events O&M consumes on this topic are all Realm-Provisioner-produced.

### 7.3.2 SNS→SQS Fan-out (§16 A60)

`iam.membership.events` consumers: `membership-audit-q` (Audit — no filter, catch-all); `membership-authz-q` (AuthZ — dept/tenant role events + `MembershipRevoked` for cache eviction); `membership-realm-q` (RP — approver make/unmake + admin/owner for `requires-mfa` realm role); `membership-notification-q` (Notification — user/admin emails + seat-overage banner); `membership-workflow-q` (Workflow — `override`/`TenantStateChanged`; no longer carries delegation events); `membership-billing-q` (Billing — `TenantSeatOverage*` only, filter policy).

Three new cross-service queues, added in this pass, consume the cascade signals introduced above (informational — these queues live in the *other* services, not this repo): `delegation-cascade-q` (Delegation Service — filters `MembershipRevoked` + `TenantMembershipsPurged`), the Tender-ACL Service's equivalent queue (same two event types), and the Group-Mapping Service's equivalent queue (filters `TenantMembershipsPurged` only).

`iam.tenant.events` consumers: `tenant-audit-q`, `tenant-notification-q` (welcome/trial-start emails), `tenant-realm-q` (Realm Provisioner — `TrialStarted` only, seeds its own local trial-expiry sweep; resolves RP-4, no batch "expired trials" endpoint exists or is planned). O&M's own `tenant-orgm-q` is separate — produce/consume disjoint.

Every subscribing queue: `-dlq`, `maxReceiveCount=5`, `processed_events` dedup.

### 7.4 CloudEvents Envelope

`{id (UUID v7), source, tenant_id, trace_id, specversion, time, subject, actor, dataschema, data}`. Governed by `platform-schemagov` — `api/asyncapi.yaml` is design-time source of truth; `schema-gov extract` writes the Draft-07 files directly into `internal/adapter/outbound/eventbus/schemas/*.json` (there is no separate `internal/eventschema/` workspace — this one directory is both the schema-gov output and the `//go:embed`-ed runtime copy); Glue Schema Registry runtime enforcement (ap-south-1). CI: `docker run ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4` for extract → validate → enforce-lifecycle → diff (PR) / register (main).

### 7.5 Event Invariants

- **EVT-4** — `processed_events` PK `(event_id, consumer)` = canonical dedup.
- **EVT-5** — `maxReceiveCount=5` → DLQ; never silent drop.
- **EVT-6** — Illegal state transitions rejected (e.g. any move off `offboarded`, PAID-1).
- **EVT-10** — Transactional outbox: business write + event share one `RunInTx`.
- **EVT-11** — Invitation lifecycle is audit-only; acceptance rides existing `TenantRoleGranted`/`DepartmentMembershipGranted` (PI-7).
- **EVT-14** — Recency guard on `tenants` projection (above).
- **EVT-15** — Future-time clamp (above).
- **EVT-16** — Tenant-state relay in same tx as projection UPDATE (above).
