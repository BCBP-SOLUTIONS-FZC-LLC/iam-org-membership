# Database Schema

Refines HLD §7.3. Database `org_membership` on shared RDS PostgreSQL 17 (Multi-AZ), fronted by PgBouncer transaction pooling. Pool: `pgcommon.NewPool(... MinConns: 0, MaxConns: 10–20, GUCProvider: pgcommon.GUCSetFromContext)`. Local dev uses `postgres:17-alpine` in docker-compose (matches sibling `iam-user-profile2`).

## Extensions and Enums (§4.1)

```sql
CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid()

CREATE TYPE tenant_plan           AS ENUM ('starter','pro','enterprise');
CREATE TYPE subscription_status   AS ENUM ('trial','active','past_due','cancelled','suspended','trial_expired','offboarded');
CREATE TYPE tenant_role           AS ENUM ('tenant_owner','tenant_admin','tender_admin','member');
-- 'member' is DERIVED-ONLY (§16 A29, TR-7): never stored; injected by I-8 into the effective role set. Kept in enum so header and derived value share a domain.
CREATE TYPE membership_status     AS ENUM ('active','suspended','left');
CREATE TYPE dept_role             AS ENUM ('preparator','reviewer','approver');
CREATE TYPE realm_type            AS ENUM ('shared','dedicated');      -- §16 A22: explicit strategy flag, never derived from realm_id string match
CREATE TYPE invitation_status     AS ENUM ('pending','accepted','expired','revoked');  -- §16 A11
```

**`branding_level`, `delegation_scope`, `delegation_status`, and `tender_acl_level` no longer exist.** `branding_level`'s only column (`plans.custom_branding`) moved to the Catalog Service with the `plans` table; `delegation_scope`/`delegation_status` moved with `delegations` to the Delegation Service; `tender_acl_level` moved with `tender_acl_entries` to the Tender ACL Service. All four were dropped in the ADR-0007/ADR-0008 decomposition and are not recreated by the current single consolidated migration (`000000_initial_schema`) — this service was never deployed, so the incremental migration history (including the migration that first added `branding_level`'s `'full'` value, and the ones that later dropped these four types) was squashed rather than carried forward as dead schema history.

**Invariant PAID-1.** `offboarded` is **terminal** — no transition out permitted. Enforced by `chk_offboarded_soft_deleted` (`status='offboarded'` requires `deleted_at IS NOT NULL`) and by explicit rejection + audit of any event/API that would move a tenant off `offboarded`. Trial lifecycle terminates at `trial_expired` → hard-delete. Paid lifecycle: `active → past_due → cancelled → suspended → offboarded`; `TenantReactivated` legal only *before* `offboarded`.

## Tables (§4.2)

8 tables total. Every soft-deletable table's uniqueness constraint is a **partial unique index** (`WHERE deleted_at IS NULL`, or `WHERE status='pending'` for `pending_invitations`) so a user can rejoin a tenant/department they previously left (the TM-11 / DM-3 / PI-1 rejoin bug fixed rev 0.17/0.18). Config tables without `deleted_at` use ordinary full `UNIQUE`.

**`plans` and `departments` no longer live in this schema** (migration-runbook Phase 4, ADR-0007 Wave 1). Both moved to the standalone Catalog / Admin Config Service (`iam-catalog-admin`); O&M reads them read-only through `port.DepartmentCatalogReader/port.PlanCatalogReader` (cached, `om:departments`/`om:plans`, 600s TTL — see `api-caching-events.md`). The 4 FKs that used to reference these tables (`fk_tenants_plan`, `fk_td_department`, `fk_dm_department`, `fk_gdm_department`) were dropped in the same migration and replaced by app-level existence checks against the catalog at the point of write (see `department_service.go`, `dept_membership_service.go`, `provisioning_service.go`). Operator write endpoints O-1/O-2/O-3/O-5/O-6 (create/patch department, patch plan) moved with the tables; only O-4 (feature-flags) and O-7 (reassign-owner) remain in O&M's `/operator` group.

**`group_dept_role_mappings`, `group_tenant_role_mappings`, and `group_dept_mappings` no longer live in this schema either** (ADR-0007 Wave 2). All three moved to the standalone Group Mapping / JIT Config Service (`group-mapping-jit-config`), which also took over the group→role/department admin CRUD endpoints (formerly P-14/P-15/P-16/P-17/P-29). I-10's JIT resolution reads them read-only through `port.GroupMappingClient` (a single consolidated GM-I1 call, cached as `om:grm`/`om:gdm`/`om:gtrm` + 24h `:stale` fallbacks, 600s primary TTL — see `api-caching-events.md`); on a cold cache and an unreachable Group Mapping Service, I-10 fails open with an empty resolution rather than failing the SAML login.

**`tender_acl_entries` (+ the `tender_acl_level` ENUM) no longer lives in this schema** (ADR-0007 Wave 3). It moved to the standalone Tender ACL Service (`iam-tender-acl`), which also took over its admin/check endpoints (formerly P-21/P-22/P-23/I-12). Dropping the table took its RLS policy, indexes, `fk_tae_tenant`/`fk_tae_tenant_membership` FKs, and touch trigger with it. Tender ACL Service now validates membership at grant time via this service's new `GET /api/v1/internal/tenants/:id/members/:user_id/exists` (I-15) instead of a composite DB FK.

**`delegations` (+ the `delegation_scope`/`delegation_status` ENUMs) no longer lives in this schema** (ADR-0008). It moved to the standalone Delegation Service (`iam-delegation`), which also took over OOO coordination and the delegation endpoints (formerly P-18/P-19/P-20/P-32/P-33) and the `delegation-expiry`/`delegation-review` CronJobs. `tenants.delegation_max_duration_days`/`delegation_review_window_days` and their `tenants_delegation_max_duration_range`/`tenants_delegation_review_window_range` CHECKs are likewise not present — that per-tenant policy now lives in Delegation Service's own `delegation_tenant_settings` table. Delegation Service validates membership at grant time via I-15 (like Tender ACL above) instead of its former composite DB FKs to `tenant_memberships`. This service's only remaining delegation touchpoint is a synchronous `GET {DELEGATION_BASE_URL}/internal/delegations/dept-delegate` precision lookup on the admin dept-membership Assign/Remove path (`port.DelegationCheckClient`), which degrades to tenant-wide impact scoping on outage rather than failing.

| Table | Key columns / notable constraints |
|---|---|
| `tenants` | Root: `id`, `slug` (immutable via `trg_tenant_slug_immutable`), `plan`, `feature_flags jsonb` (override delta only; §16 A18/T-9), `status`, `trial_ends_at`, `trial_reactivation_count` (CHECK 0..1, T-14), `subscription_started_at`, `cancelled_at` (T-11), `suspension_source` (T-16, new — `billing_lapse`\|`operator`, resolves RP-11), `last_event_at` (EVT-14 high-water mark), `realm_id`, `realm_type` (§16 A22, drives partial-unique on realm_id), `keycloak_shard` (§16 A23, T-12; RP-owned projection), `mfa_freshness_seconds` (T-10, 60–900 default 300), `local_accounts_enabled`, `realm_sync_pending` (T-15), `default_locale`, `licensed_seats` (SEAT-1..SEAT-5), `ownerless_since` (T-13), `overage_since` (SEAT-5), `record_version`. `uq_tenants_slug` (full unique), `uq_tenants_realm_id_dedicated WHERE realm_type='dedicated'`. Partial indexes: `idx_tenants_ownerless`, `idx_tenants_realm_sync_pending`, `idx_tenants_seat_overage`. Checks: `chk_trial_ends_at_required`, `chk_subscription_started_required` (exempts `suspended` too, T-16), `chk_offboarded_soft_deleted` (PAID-1), `chk_cancelled_at_required` (T-11, CASE form — the `suspended` branch is conditional on `suspension_source`, T-16), `chk_suspension_source_required` (T-16, two-directional). `plan` no longer has a DB FK (`fk_tenants_plan` dropped, Phase 4) — validity against the Catalog Service's plan set is an app-level concern where enforced. |
| `tenant_departments` | Composite PK `(tenant_id, department_id)`; join activating a catalog dept (now Catalog-Service-owned) for a tenant; `is_active`. `department_id` no longer has a DB FK (`fk_td_department` dropped, Phase 4) — existence is checked against `port.DepartmentCatalogReader` in `department_service.go` before activation. |
| `tenant_memberships` | Lifecycle only, **no role data** (§16 A14). `status ENUM (active|suspended|left)`. `uq_tm_active_user` = `UNIQUE (tenant_id, user_id) WHERE deleted_at IS NULL` (TM-1/TM-11 rejoin). |
| `tenant_roles` (§16 A14) | Elevated grants only: `tenant_owner`/`tenant_admin`/`tender_admin`. Multi-role support (TR-1, one row per grant). `tenant_membership_id` composite FK `(id, tenant_id, user_id) → tenant_memberships` (§16 A31, TR-8). `uq_tenant_roles_active` = `UNIQUE (tenant_id, user_id, role_code) WHERE deleted_at IS NULL` (TR-2). `chk_tr_no_member` bars `role_code='member'` (TR-7). `granted_by` (audit). |
| `dept_memberships` | User↔dept↔`role_level` (preparator/reviewer/approver). `tenant_membership_id` composite FK `(id, tenant_id, user_id) → tenant_memberships` (§16 A15/A28, `fk_dm_tenant_membership`). `uq_dm_active_membership` = `UNIQUE (tenant_id, user_id, department_id) WHERE deleted_at IS NULL` (DM-3). `granted_by` audit-only (§16 A32(e), DM-5); `iam-system` for JIT/acceptance. `department_id` no longer has a DB FK (`fk_dm_department` dropped, Phase 4) — checked in `dept_membership_service.go::Assign` against `port.DepartmentCatalogReader`. |
| `dept_role_labels` | Per-tenant customizable display labels for the 3 dept-role rungs. `uq_dept_role_labels = UNIQUE (tenant_id, role_code)` (DRL-1). Only `display_name` is mutable via P-13. Historically named `tenant_roles` — renamed rev 0.97 to free that name for the actual tenant-level role table (§4.2 head note). |
| `pending_invitations` (new §16 A11) | Two-step invite→accept staging. **One PII exception** (email `citext` + `full_name`; §15.8). `uq_pi_pending = UNIQUE (tenant_id, email) WHERE status='pending'` (PI-1). `initial_tenant_roles tenant_role[]`, `initial_dept_mappings jsonb`, `keycloak_user_id`, `expires_at` (7 d; = Keycloak invite action-token lifespan, coupled via `INVITATION_EXPIRY_DAYS`), `kc_cleanup_pending boolean` (saga-compensation marker for the reconciler, PI-9). |
| `processed_events` | Consumer-idempotency ledger. PK `(event_id, consumer)`. **Global, no tenant scoping, no FK, no RLS.** 8-day retention (PE-1 strictly > 7-day SQS message lifetime; IDEMP-4 backstopped by EVT-14/PI-10/IDEMP-3 for beyond-window duplicates). `idx_processed_events_prune (processed_at)`. |

## Row-Level Security (§4.3)

Every tenant-scoped table (all except `processed_events` — the 7 tenant-scoped tables are `tenants`, `tenant_departments`, `tenant_memberships`, `tenant_roles`, `dept_memberships`, `dept_role_labels`, `pending_invitations`) has:
```sql
ENABLE ROW LEVEL SECURITY;
FORCE ROW LEVEL SECURITY;
REVOKE ALL FROM PUBLIC;
CREATE POLICY tenant_isolation FOR ALL
  USING      (rls_check_tenant(tenant_id, 'table_name'))
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);
```
(Re-verified directly against `internal/adapter/outbound/postgres/migrations/000000_initial_schema.up.sql` and `test/postgres/rls_test.go`'s `TestRLS_Case1_EveryTenantScopedTableEnabled`, which asserts `relforcerowsecurity = true` for exactly these 7 tables — both `ENABLE` and `FORCE` are separate `ALTER TABLE` statements in the migration, double-spaced for column alignment, which is why a naive single-space grep for the literal string can miss the `FORCE` lines.)
**Special case — `tenants` table.** Policy matches `id = current_setting('app.tenant_id')::uuid` (a tenant can only read/write its own row).

**Cross-tenant admin access** requires the `BYPASSRLS` role **`org_membership_migrator`** (never the app role `org_membership_app` — CI-verified). I-16's `ListSubscriptionLapses` (§16 OQ-9/RP-C3) is the one HTTP-served read that needs this — its `TenantRepository` instance is constructed against `sysPool`, not the RLS-scoped app pool, the same BYPASSRLS binding the reconciler jobs and business-metric exporters already use.

**Provisioning writes** use the reserved system principal `iam-system` (`…00a1`) with the **target tenant's** `x-tenant-id`, so `GUCSet{UserID: system, TenantID: target}` makes the new row's `WITH CHECK` pass. Accepted **only** on `/api/v1/internal/*` routes (RLS-5, IAPI-2).

**RLS invariants:**
- **RLS-1** — Every tenant-scoped table runs `ENABLE + FORCE` + policy + `REVOKE ALL FROM PUBLIC` (fail-closed).
- **RLS-2** — Missing/malformed GUC → 0 rows, no writes (`current_setting(..., true)` is NULL → no row matches).
- **RLS-3** — Cross-tenant `INSERT`/`UPDATE` rejected by `WITH CHECK`.
- **RLS-4** — Only `org_membership_migrator` holds `BYPASSRLS`. Operator *domain* actions go through service-layer `platform_operator` check, not a DB bypass.
- **RLS-5** — Internal provisioning under system principal + target `x-tenant-id` only on `/api/v1/internal/*`.
- **RLS-6** — GUC is **transaction-local** on every checkout (writes AND reads). `SET LOCAL` semantics via `set_config(..., is_local => true)`. Auto-resets at `COMMIT`/`ROLLBACK` — can never leak across a pooled backend. **Verified** by RLS test Case 5 (§14.5): tenant A tx → connection returns to pool → tenant B tx on same backend → B sees 0 of A's rows. CI additionally greps for session-scoped `SET app.tenant_id` as a forbidden pattern.

## Triggers — `touch_row()` (§4.5)

```sql
CREATE OR REPLACE FUNCTION touch_row() RETURNS trigger AS $$
BEGIN
  NEW.updated_at     := now();
  NEW.record_version := OLD.record_version + 1;
  RETURN NEW;
END; $$ LANGUAGE plpgsql;
```

`BEFORE UPDATE ... FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)` on every `record_version`-carrying table. `processed_events` excluded (no `record_version`).

Additional triggers on `tenants`:
- `trg_tenant_slug_immutable` — `BEFORE UPDATE OF slug` raises hard exception on change.

**Trigger invariants (TRG-1..3):**
- **TRG-1** — Client code never sets `record_version`; the trigger owns it. Optimistic-lock `UPDATE ... WHERE id=$1 AND record_version=$2` reads the version in WHERE, never in SET.
- **TRG-2** — `updated_at` and `record_version` are DB-managed; client-supplied values are overwritten.
- **TRG-3** — No-op update (`OLD.* IS NOT DISTINCT FROM NEW.*`) suppresses trigger — no version/timestamp bump, no spurious optimistic-lock conflicts for concurrent readers.

## Migrations (§4.4)

- Forward-only, additive. `migrate.Runner{DSN}.Up(ctx)` at startup. `outbox.ApplySchema` runs **before** business migrations (MIG-2) — the domain migration alters `outbox_events.payload` from jsonb to text, so platform-events must have already created that table first.
- Uses `MIGRATION_DATABASE_URL` (direct Postgres connection, **bypasses PgBouncer** — CONFIG-2; DDL incompatible with transaction pooling).
- `migrate.Runner` with `lock_timeout=30s`.
- **MIG-1 additive-then-destructive split** — Destructive change (column/table drop, type narrowing) ships in a **separate, later release** only after application no longer reads/writes the object. Applied historically to: `tenant_memberships.top_role` drop (§16 A14), `tenant_roles.keycloak_group_name` (rev 0.22), `tender_acl_entries.access_level` ENUM conversion (§16 A17) — `tender_acl_entries` itself has since been dropped entirely (moved to Tender ACL Service). This discipline governs *future* schema changes; the ADR-0007/ADR-0008 decomposition itself was squashed into the single `000000_initial_schema` migration rather than preserved as incremental history, since this service has never been deployed (no live data/traffic to protect).
- **MIG-3 / MIG-5** — Only `org_membership_migrator` has `BYPASSRLS`; CI-verified `org_membership_app` does NOT.
- **MIG-4** — CI verifies every tenant-scoped table retains `rowsecurity = true AND forcerls = true`.
- **MIG-6** — Backward-compatible during rollout (mixed replicas).
- **MIG-7** — No blocking rewrites or long locks in production. Volatile `DEFAULT`s decomposed.
- **MIG-8** — New `UNIQUE`: `CREATE UNIQUE INDEX CONCURRENTLY` in one release, `ALTER TABLE ... ADD CONSTRAINT ... USING INDEX` in the next.
- **MIG-9a** — Changing a partial index's `WHERE` predicate is drop-and-recreate `CONCURRENTLY`, never `ALTER INDEX`.
- **MIG-9b** — New FK or CHECK on populated tables: `ADD CONSTRAINT ... NOT VALID` first, `VALIDATE CONSTRAINT` second. Applied to `dept_memberships.fk_dm_tenant_membership` (§16 A15) and `tenants.chk_cancelled_at_required` (§16 A24).
- **`NOT NULL` with constant DEFAULT is single-step** (§19.2, Postgres 11+ stores as catalog metadata, no row rewrite). Applied to `licensed_seats`, `feature_flags`, `keycloak_shard`.
- Fan-out migrations: batched CronJob 1000 rows/tx with `ON CONFLICT DO NOTHING` (system-dept backfill, `dept_memberships.tenant_membership_id` backfill).

## Tenant Invariants (T-1..T-16)

- **T-1** — `slug` unique + immutable.
- **T-2** — `realm_id` never NULL/empty; trial → `'trial'` shared realm; paid → dedicated. Which case is driven by `realm_type`, never string-match.
- **T-3** — `users.locale` = User Profile; `tenants.default_locale` = O&M. `default_currency` is **not** an O&M column (Billing-owned, HLD §10.7, §16 A32(b)).
- **T-4** / **T-5** — Trial states carry `trial_ends_at`; paid states carry `subscription_started_at`.
- **T-6** — Dedicated realm names globally unique (`uq_tenants_realm_id_dedicated WHERE realm_type='dedicated'` — deliberately **no `AND deleted_at IS NULL`**; a soft-deleted-but-not-yet-hard-deleted realm still exists in Keycloak and its name must remain reserved).
- **T-7** — Soft-deleted tenants stay queryable for audit FK integrity; no app-code hard-delete.
- **T-8** — `licensed_seats > 0` always.
- **T-9** — `feature_flags` holds only the override delta; effective set `planDefaults(plan) ∪ feature_flags` computed at read time. Plan changes never touch the column. Writable only via `platform_operator` (O-4).
- **T-10** — `mfa_freshness_seconds ∈ [60, 900]`. AuthZ Enrichment sources it from the tenant cache `om:tenant` (evicted by P-2 write), not from the frozen per-user `om:memberships` snapshot (§16 A52).
- **T-11** — `cancelled_at IS NOT NULL ⇔ status ∈ (cancelled, offboarded)` **or** `status = suspended AND suspension_source = billing_lapse` (refined by T-16, resolves RP-11). Set/cleared in lockstep with status by `TenantSubscriptionCancelled`/`TenantReactivated`.
- **T-12** — `keycloak_shard` is RP-owned projection; O&M stores, never selects. At MVP all `'shard-0'`.
- **T-13** — `ownerless_since` set only by TM-12 (I-5 removing last active `tenant_owner`); cleared only by O-7 (operator reassignment). Backs `iam_tenant_ownerless` alert.
- **T-14** — `trial_reactivation_count ∈ [0, 1]` schema-backstops HLD Invariant TRIAL-5 (one-time reactivation cap).
- **T-15** — `realm_sync_pending` marks Option A (local-first, commit-then-call) reconciliation for realm-affecting settings. Set when P-2's synchronous RP `PatchRealmConfig` fails (endpoint returns 202); cleared by `realm-config-sync` reconciler (§13.1). Disable direction (`local_accounts_enabled → false`) prioritized (security-tightening).
- **T-16** (new, resolves RP-11) — `suspension_source ∈ {billing_lapse, operator}`, non-NULL iff `status = suspended` (two-directional CHECK, same lockstep discipline as `cancelled_at`/T-11). `billing_lapse` is the normal cancelled→suspended path (`cancelled_at` also set); `operator` is an RP-14 administrative suspension that can land directly from `active`/`trial`, skipping the grace path — `cancelled_at` is deliberately left NULL on this branch so the tenant never enters the §15.5 grace/retention clock. `chk_subscription_started_required` (T-5) exempts `suspended` from requiring `subscription_started_at`, so an operator can suspend a never-converted trial tenant. Cleared to NULL by `TenantReactivated` alongside `cancelled_at`, regardless of which path led to `suspended`.

## Seat Invariants (SEAT-1..SEAT-5, §16 A10)

- **SEAT-1** — Hard cap enforced transactionally. `POST /tenants/:id/members` (P-6) inside `RunInTx`: `SELECT ... FOR UPDATE` on tenants row, count `active_memberships + pending_invitations(status='pending' AND expires_at > now())`, reject at/above `licensed_seats` with `409 seat_limit_reached`. HLD §8.2.2 formula.
- **SEAT-2** — `licensed_seats` is a **Billing projection**. O&M never rejects an update to it — accepts every value pushed by `TenantSeatsChanged`, even a decrease that puts the tenant over cap.
- **SEAT-3** — On over-cap: **existing users keep access**, new invites blocked immediately (SEAT-1), grace window opens (`grace_ends_at = overage_since + SEAT_OVERAGE_GRACE_DAYS`, default 30 d). O&M **never auto-removes or suspends** — post-grace enforcement is Billing's (SEAT-4). Over-cap self-heals as invites lapse (PI-5) or admins remove users.
- **SEAT-4** — O&M never originates `licensed_seats` (no tenant-admin endpoint) and never decides overage enforcement (Billing decides off `TenantSeatOverageStarted`).
- **SEAT-5** — `overage_since` set/cleared inline under the tenant row lock by every SEAT-1-triple mutator (member add/remove, invite create/expire/revoke/accept, seats change). Daily `seat-overage-reconcile` CronJob backstop. Emits `TenantSeatOverageStarted` / `TenantSeatOverageResolved` on transitions.

## Concurrency Invariants (CONC-1..4)

- **CONC-1** — All 7 `record_version`-carrying tables use optimistic locking: `tenants`, `tenant_departments`, `tenant_memberships`, `tenant_roles`, `dept_memberships`, `dept_role_labels`, `pending_invitations`. (`departments`/`plans` moved to the Catalog Service in Phase 4; `group_dept_role_mappings`/`group_tenant_role_mappings`/`group_dept_mappings` moved to Group Mapping Service in Wave 2; `tender_acl_entries` moved to Tender ACL Service in Wave 3; `delegations` moved to Delegation Service — see the schema-ownership notes above.) `processed_events` excluded.
- **CONC-2** — Success only when stored `record_version` equals client's expected.
- **CONC-3** — `RowsAffected() == 0` on versioned UPDATE returns `409 optimistic_lock_conflict`.
- **CONC-4** — 409 response includes current `record_version` + `updated_at` for retry.

## Idempotency (IDEMP-1..4, §9.2)

- **IDEMP-1** — Every replayable operation converges to the same final state.
- **IDEMP-2** — `processed_events` PK `(event_id, consumer)` is the canonical bus-event dedup (EVT-4).
- **IDEMP-3** — UPSERTs (`INSERT ... ON CONFLICT DO UPDATE`) on natural identity keys + partial unique indexes collapse replays onto the same row.
- **IDEMP-4** — 8-day dedup window is deliberate; beyond-window duplicates (SQS max 14 d + DLQ dwell) are backstopped by **value-level** guards: EVT-14 recency guard (stale skip), PI-10 acceptance idempotency, IDEMP-3 UPSERT convergence.

## Data Model Entities NOT Owned Here

- `assignee_overrides` (workflow-instance/node reassignment record) — Workflow Service owns it. O&M validates + emits `TenderAssigneeOverridden` via I-13 but persists nothing (§2.2, §16 A32(d), OVR-1).
- `plans`, `departments` (global catalogs) — Catalog / Admin Config Service. O&M reads them read-only via `om:plans`/`om:departments`.
- `group_dept_role_mappings`, `group_tenant_role_mappings`, `group_dept_mappings` — Group Mapping / JIT Config Service. O&M reads a consolidated resolution via `om:grm`/`om:gdm`/`om:gtrm`.
- `tender_acl_entries` — Tender ACL Service. O&M's only touchpoint is serving grant-time membership checks to it via I-15.
- `delegations` (+ its tenant-level `delegation_max_duration_days`/`delegation_review_window_days` policy columns) — Delegation Service. O&M's only touchpoints are the department-scope precision lookup via `port.DelegationCheckClient` and serving I-15 membership checks to it.
- Metered quota counters — Usage & Metering Service (HLD §10.6, §16 A26). O&M stores static plan + feature-flag override only.
- `default_currency` — Billing (§16 A32(b)).
- Display identity, signature, OOO presentation flag — User Profile.
- Credentials, MFA enforcement, JWT — Keycloak.
