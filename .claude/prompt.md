Build Plan — iam-org-membership (Greenfield Go Service)

Keyed to Org & Membership LLD v1.61 sections and invariant IDs. This is a plan for confirmation — no code yet. One reviewable PR per phase; stop for review after each.

Conventions locked from iam-user-profile2 (sibling — canonical)

Confirmed by reading its go.mod, cmd/server/main.go, Makefile, Dockerfile, deploy/helm/, and migrations:

- Go 1.26.5, platform-gincommon v1.2.0, platform-pgcommon v1.1.1, platform-events v1.3.0.
- Clean-arch names: internal/core/{domain,port,service} + internal/adapter/{inbound/{http,consumer},outbound/{postgres,eventbus,valkey,metrics,userprofile,workflow,realmprovisioner}} + internal/eventschema/ + pkg/requestctx/.
- Migrations: platform-pgcommon migrate.Runner, embed internal/adapter/outbound/postgres/migrations/*.sql, six-digit NNNNNN_<name>.{up,down}.sql, MIGRATION_DATABASE_URL bypasses PgBouncer.
- Pool: pgcommon.NewPool with GUCProvider: pgcommon.GUCSetFromContext — the transaction-local SET LOCAL app.tenant_id (RLS-6) is a library concern, not app code.
- Outbox: outbox.ApplySchema after business migrations; TxRunner from postgres/db.go injects a tx-bound publisher into context.
- No internal/config/ package — env read inline via envOr + validateRequiredEnv in main.go.
- Logger: Zap via gincommon/pkg/logger. Metrics: prom client_golang, iam_* prefix.
- Errors: single internal/core/domain/errors.go with DomainError{Code, Message, Cause} + sentinel Err…, HTTP mapping in an http/middleware.go handleError.
- Test layout: test/{unit,postgres,integration,e2e,fixtures} with the integration build tag; testcontainers-go.
- Dockerfile: multi-stage, digest-pinned golang builder → distroless runtime.
- Helm chart at deploy/helm/; CronJobs as separate template files.
- CI: ci.yml fan-in to reusable validate-quality.yml + validate-test.yml; separate schema-registry workflow using platform-schemagov.

Deviations from sibling to plan for:

1. RoutingPublisher (two topics) — sibling has a single SNS_TOPIC_ARN publisher. O&M needs events.NewRoutingPublisher keyed off Envelope.Source mapping to iam.membership.events + iam.tenant.events (arch doc line 118–126).
2. 11 CronJobs vs sibling's 1 — see Phase 5. Each is either a separate cmd/<name>/main.go binary or a helm CronJob template invoking the server with a --job= flag; I'll follow the sibling pattern of a second binary (cmd/reconciler/main.go) that dispatches by env/flag, given the sibling proves that mold. Confirming this choice is one of my open questions below.
3. platform-schemagov version: sibling ships :0.4, our .claude/CLAUDE.md pins v0.3.0. Confirming which is authoritative.

---
Phase 0 — Scaffolding (LLD §3, §12)

Goal: A repo that compiles, boots, reports health, connects to DB with RLS binding, and has CI green. Zero business logic.

- [ ] go.mod module github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership, Go 1.26.5, pin platform libs to sibling versions.
- [ ] Repo layout per .claude/architecture.md — top-level cmd/, internal/{core/{domain,port,service},adapter/{inbound/{http,consumer},outbound/{postgres,valkey,eventbus,userprofile,workflow,realmprovisioner,metrics}}}, internal/eventschema/, api/{openapi.yaml,asyncapi.yaml}, test/, pkg/requestctx/.
- [ ] .go-arch-lint.yml enforcing §3 rules (core/domain → core/domain only; core/service → core/domain,core/port,pkg/requestctx; adapter/* implements port; no core/ → adapter/).
- [ ] .golangci.yml mirrored from sibling.
- [ ] Makefile targets mirrored (§CLAUDE.md "Common Commands"): setup, tidy, fmt, fmt-check, vet, lint, test-unit, test-integration, test-e2e, test-smoke, test, test-ci, docker-down, cover, cover-func, ci, mod-verify, vuln-check, extract-schemas, schema-*, swag, install-hooks, clean.
- [ ] Dockerfile multi-stage, digest-pinned, ENTRYPOINT builds both cmd/server and cmd/reconciler.
- [ ] docker-compose.yml + .env.local.yml for Postgres + PgBouncer + Valkey + LocalStack.
- [ ] .env-example with every §12 env var (see below).
- [ ] cmd/server/main.go bootstrap: env validation (validateRequiredEnv), zap logger, gin release mode, gincommon.Init, pgcommon.NewPool{GUCProvider: pgcommon.GUCSetFromContext, SlowQueryThreshold=200ms} (arch line 108–116), sysPool for BYPASSRLS work (RLS-4), migrations then outbox.ApplySchema (MIG-2), Valkey port.Cache, AWS clients (SQS/SNS/Glue), Noop codec + ValidatingCodec wired but active only when GLUE configured, router with TimeoutMiddleware(30s) + ObservabilityMiddlewares + HealthHandler + /readyz + /metrics + Swagger.
- [ ] Shutdown order per sibling: HTTP server drain → bg ctx → sqs consumer stop → cache close → tracing/logger shutdown.
- [ ] Health/readiness: /healthz = gincommon.HealthHandler + cache.Health + outboxRunner.Ready() (cache is advisory — degrade but stay ready per CACHE-9).
- [ ] pkg/requestctx/ typed RequestContext.
- [ ] internal/adapter/inbound/http/middleware.go: GUCBridgeMiddleware writes TenantID/UserID/Roles into pgcommon.GUC context.
- [ ] .github/workflows/: ci.yml, validate-quality.yml, validate-test.yml, dependency-check.yml, release.yml, schema-registry.yml, schema-prune.yml, schema-health-quarterly.yml, freeze-watchdog.yml — mirrored from sibling.
- [ ] README.md, CLAUDE.md (keep the existing .claude/* docs), ARCHITECTURE.md, CONTRIBUTING.md, CHANGELOG.md stub.

Phase 0 tests: go build, make lint, make vet, docker compose up, service comes up green on /healthz and /readyz.

---
Phase 1 — Schema & Migrations (LLD §4, §19)

Goal: All 15 tables, enums, indexes, RLS policies, triggers and reversible.

Migration ordering (per §19.5 hard dependencies)

Numbered 000001…, one concern per file, up + down pair

- [ ] 000001_extensions.up.sql — citext, pgcrypto.
- [ ] 000002_enums.up.sql — all 11 enums per §4.1 (tenant_plan, subscription_status, tenant_role, membership_status, delegation_scope, delegation_status, tender_acl_level, realm_type, tenant_status, branding_level).
- [ ] 000003_plans_catalog.up.sql — plans (PK code, §16 A19 columns). Seed the 3 tiers (starter, pro, enterprise). Must precede tenants because of fk_tenants_plan.
- [ ] 000004_departments_catalog.up.sql — departments (global, immutable code/is_system, chk_system_department_active). Seed the 5 system departments (§8.1 trial signup). Must precede tenant_departments.
- [ ] 000005_tenants.up.sql — tenants full column set (slug, plan FK, feature_flags, status, trial_ends_at, trial_reactivated_at, trial_conversion_ratio CHECK 0..1, subscription_started_at, cancelled_at, last_event_at, record_version, realm_type, keycloak_shard, mfa_freshness_seconds 60–900 default 300, local_accounts_enabled, realm_sync_pending, default_locale, licensed_seats, ownerless_since, overage_since, checks (chk_trial_ends_at_required, chk_subscription_started_at_required, chk_offboarded_soft_deleted, chk_cancelled_at_required), uq_tenants_slug, uq_tenants_realm_id_dedicated WHERE realm_type='dedicated' (no deleted_at filter per T-6), partial indexes ownerless/realm_sync_pending/seat_overage.
- [ ] 000006_tenant_memberships.up.sql — lifecycle-only, no role columns (§16 A14). uq_tm_active_user WHERE deleted_at IS NULL (TM-1/TM-11). Composite unique uq_tm_id_tenant_user (id, tenant_id, user_id) required target for A15/A28/A16/A31 composite FKs (§19.5 must precede any table referencing it).
- [ ] 000007_tenant_departments.up.sql — composite PK.
- [ ] 000008_tenant_roles.up.sql — §16 A14 multi-role; composite FK tenant_membership_id → (id, tenant_id, user_id) (uq_tenant_roles_active WHERE deleted_at IS NULL (TR-2)).
- [ ] 000009_dept_memberships.up.sql — composite FK fk_dm_tenant_membership (§16 A15/A28); uq_dm_active_membership WHERE deleted_at IS NULL (DM-3).
- [ ] 000010_dept_role_labels.up.sql — uq_dept_role_labels (tenant_id, role_code) (DRL-1).
- [ ] 000011_group_mappings.up.sql — three tables: group_dept_role_mappings (§16 A25 rename), group_tenant_role_mappings (§16 A25 new, chk_gtrm_no_member), group_dept_mappings. Each with its uq_* per GDRM-1 / GTRM-1 / GDM-1.
- [ ] 000012_delegations.up.sql — composite FKs on delegator/delegate memberships; del-9 both parties same tenant); idx_delegations_delegator and idx_delegations_ends_at (hot paths).
- [ ] 000013_tender_acl_entries.up.sql — composite FK lifecycle; level ENUM (view|edit|approve) (§16 A32(c)/rev 1.27 HLD-aligned); uq_tae_active_entry WHERE deleted_at IS NULL (TAE-1); granted_by NOT NULL, reason (nullable 500 char), expires_at (future-only at handler).
- [ ] 000014_pending_invitations.up.sql — §16 A11; PII exception: email citext, full_name; uq_pi_pending WHERE status='pending'; initial_tenant_roles tenant_role[], initial_dept_mappings JSONB, expires_at, kc_cleanup_pending (PI-9).
- [ ] 000015_processed_events.up.sql — PK (event_id, consumer). No RLS, no tenant scoping, no FK; idx_processed_events_processed_at(processed_at).
- [ ] 000016_triggers.up.sql — touch_row() function; BEFORE UPDATE ... WHEN (OLD.* IS DISTINCT FROM NEW.*) on all 14 tenant tables (TRG-1/2/3); trg_tenant_slug_immutable; BEFORE DELETE guards for system departments (D-3, D-9).
- [ ] 000017_rls.up.sql — On all 12 tenant-scoped tables: ENABLE + FORCE ROW LEVEL SECURITY, REVOKE ALL FROM PUBLIC, tenant_isolation (RLS-1). Uses rls_check_tenant(tenant_id) in USING and WITH CHECK. Special case: tenants policy is id = current_setting('app.tenant_id')::uuid.
- [ ] 000018_roles.up.sql — DB roles: org_membership_app, org_membership_migrator (BYPASSRLS). CI test grep + pg_roles assertion (RLS-4, MIG-3/5).

Down migrations: DROP CASCADE for each up. MIG-1 additive-then-destructive isn't triggered in the greenfield initial cut (nothing to destructively remove).

Phase 1 tests:

- Postgres: schema comes up clean; enum values match §4.
- RLS Case 1–5 (§14.5): fail-closed on missing GUC (RLS-2), WITH CHECK on cross-tenant insert (RLS-3), BYPASSRLS only internal-provisioning under system principal + target tenant (RLS-4/5), no cross-tenant leak across a pooled PgBouncer connection (RLS-6, canonical test).
- touch_row fires only on IS DISTINCT FROM change (TRG no-op update).
- trg_tenant_slug_immutable raises on slug change (T-1).
- Composite FK rejects (id, wrong_tenant_id, user_id).
- chk_offboarded_soft_deleted enforces PAID-1.
- chk_trial_ends_at_required + chk_subscription_started_at_required (T-4/T-5/T-11).
- chk_tr_no_member and chk_gtrm_no_member reject member (TR-7, GTRM-6).
- CI has a grep for non-LOCAL SET app.tenant_id as a failure.

---
Phase 2 — Core CRUD + RLS + AuthZ (LLD §5 public, §10, §17)

Goal: All P-* public endpoints, full role gating (AUTH-1..8), the §17 error taxonomy, cache reads + post-commit invalidation, optimistic locking round-tripped.

Grouped by service:

Tenant service (P-1, P-2)

- [ ] internal/core/domain/tenant.go, tenant_repository.go.
- [ ] GET /tenants/:id (P-1) — same-tenant member (AUTH-1); cache om:tenant:{tenant} 600s.
- [ ] PATCH /tenants/:id (P-2) — tenant_owner (AUTH-1 target); default_locale, local_accounts_enabled, mfa_freshness_seconds (T-10 range check → 422 invalid_mfa_freshness_seconds); optimistic-lock round-trip (CONC-1..4, 409 optimistic_lock_conflict); evict om:tenant and om:locale; Option A local-first + RP reconciled change (T-15) — if RP PatchRealmConfig returns non-200, set realm_sync_pending=true in same tx.

Department service (P-3, P-24, P-25, P-9)

- [ ] POST /tenants/:id/departments (P-24), PATCH /tenants/:id/departments/:dept_id (P-25) — activate/deactivate/reactivate custom department; catalog immutability (D-2); 422 cannot_delete_system_department.
- [ ] GET /tenants/:id/departments (P-3) — cached.
- [ ] GET /tenants/:id/departments/:dept_id/members (P-9) — cached.

Membership service (P-4/5/6/7/8, P-27, P-28, P-30/31)

- [ ] internal/core/service/membership_service.go (thick service; §8.8, §8.10).
- [ ] GET /tenants/:id/members (P-4) — cursor pagination (§16 A4); cache page-1 only limit=50 (CACHE-10).
- [ ] GET /tenants/:id/members/:user_id (P-5).
- [ ] POST /tenants/:id/members (P-6) — Invite (two-step), 202. Inside RunInTx under SELECT ... FOR UPDATE on tenants row: compute active_memberships + pending(status='pending' AND expires_at > now()); 409 seat_limit_reached at/above cap. Also PI-11/PI-12 rate limits → 429 reinvite_too_soon / invite_rate_limited. Insert pending_invitations, call RP CreateInvitedUser, emit InvitationCreated (audit-only per EVT-11), invalidate om:seat_usage.
- [ ] PATCH /tenants/:id/members/:user_id (P-7) — suspend/reactivate (status only). AUTH-8 privilege-reduction: commit then best-effort RP RevokeUserSessions (fail-open, TTL cache).
- [ ] DELETE /tenants/:id/members/:user_id (P-8) — delegate-impact gated (§8.8). Sync call to WorkflowClient.GetDelegateImpact; if active_workflows > 0 → 409 workflow_resolution_required {workflow_ids, allowed_actions:[replace_delegate,stop_workflows]} (WFI-3). Otherwise remove: soft-delete tenant_memberships, cascade revoke tenant_roles + dept_memberships + delegations (ended_reason='delegate_removed', DEL-7), last_owner_removal, TM-12 escalation if last owner (ownerless_since), emit TenantRoleRevoked × N + DepartmentMembershipRevoked × N + DelegationEnded. Then AUTH-8 RP RevokeUserSessions fail-open.
- [ ] POST /tenants/:id/users/:user_id/removal-resolution (P-26) — {action: replace_delegate|stop_workflows, ...}; §8.8 flow; scope by delegation_id for dept-level (WFI-11).
- [ ] PUT /tenants/:id/members/:user_id/roles (P-28) — full-replacement multi-role reconcile; TM-8 last-owner guard; TenantRoleGranted / TenantRoleRevoked per role (§16 A14); AUTH-8 on de-privilege.
- [ ] GET /tenants/:id/seat-usage (P-27) — same handler used by I-11; cache om:seat_usage:{tenant} 30s (CACHE-5).
- [ ] GET /tenants/:id/invitations (P-30) / DELETE /tenants/:id/invitations/:invitation_id (P-31) — revoke sets kc_cleanup_pending=true (PI-6 → reconciler in Phase 5).

Dept membership (P-10, P-11)

- [ ] PUT /tenants/:id/departments/:dept_id/members/:user_id (P-10) — decrease is dept-scope delegate-impact gated (§8.8 delegation_id).
- [ ] DELETE /tenants/:id/departments/:dept_id/members/:user_id (P-11) — dept-scope gated (§8.8.4).

Role labels + Group mappings (P-12/13, P-14/15, P-16/17, P-29)

- [ ] Straight reads/writes with cache invalidation. P-13 mutates display_name only (DRL-1).
- [ ] P-29 (PUT .../group-mappings/tenant-roles) — new endpoint.

Delegation (P-18/19/20)

- [ ] Straightforward for now — full coordination flow with UserProfileClient. Wire the endpoints and validation (self_delegation, delegation_window_inverted, scope_id_required, invalid_delegate for inactive delegate — DEL-1 pre-flight); persistence + outbox event go behind the port in Phase 4.

Tender ACL (P-21/22/23)

- [ ] POST with optional reason, expires_at (future-only); GET active grants SQL.
- [ ] 422 invalid_expires_at.

Middleware / error taxonomy (§17)

- [ ] handleError maps every DomainError code to HTTP status (400/401/403/404/409/422/429/503).
- [ ] AUTH-6 operator route re-check applied — but no operator routes exist yet in Phase 2. Deferred to Phase 4.
- [ ] RequireSystemRole() middleware equivalent for /internal paths (AUTH-4).

Phase 2 tests:

- Unit: handler role gating for every AUTH-* rule; validation; last-owner guard; optimistic-lock 409 shape.
- Integration (testcontainers Postgres): SEAT-1 concurrency race (two invites, one seat, FOR UPDATE serializes); TM-1 uniqueness under concurrent P-28 (only one succeeds); touch_row no-op save; rejoin bug regressions (TM-11 / DM-3 / TAE-1 / PI-1 partial-index correctness).
- Cache: post-commit DEL verified; CACHE-9 read-miss fallback.

---
Phase 3 — Events: producers + consumers (LLD §7, §9)

Goal: Outbound producers with RoutingPublisher + outbox; inbound tenant-orgm-q/billing-orgm-q consumers with EVT-14/15/16 guards; schema-governance workspace populated.

Schemas (§7.4, §7.3.1)

- [ ] api/asyncapi.yaml — AsyncAPI 3.0, two channels (iam.membership.events, iam.tenant.events), every event registered.
- [ ] internal/eventschema/*.json — one Draft-07 file per event: DepartmentMembershipGranted, DepartmentMembershipRevoked, DepartmentMembershipLevelChanged, TenantRoleGranted, TenantRoleRevoked, DelegationStarted, DelegationEnded (with ended_reason ∈ {expired, cancelled, delegate_removed} per DEL-7), TenderAssigneeOverridden, TenantCreated, TrialStarted, TenantSeatOverageStarted, TenantSeatOverageResolved, TenantStateChanged (§16 A61).
- [ ] make extract-schemas (via schema-gov extract) generates from AsyncAPI; --check variant in CI catches drift.

Producer wiring (§7.3, arch line 118–126)

- [ ] internal/adapter/outbound/eventbus/publisher.go — events.NewRoutingPublisher{TopicARNs: {"iam.membership.events": ..., "iam.tenant.events": ...}} selecting by Envelope.Source.
- [ ] ValidatingCodec (fail-closed against internal/eventschema/*.json) wrapping Glue or Noop codec.
- [ ] Outbox insertion via TxRunner/EventPublisherFromContext so publish is atomic with domain write (EVT-10, CONS-1..4).
- [ ] Every Phase-2 mutation emits its LLD-mandated event(s). Multi-role reconciles emit one event per role_code (§16 A14).
- [ ] Outbox runner in main.go (env-driven: OUTBOX_POLL_INTERVAL, OUTBOX_BATCH_SIZE, OUTBOX_MAX_ATTEMPTS, OUTBOX_PUBLISH_CONCURRENCY, etc.).

CloudEvents envelope (§7.4)

- [ ] id (UUID v7), source="iam-org-membership", tenant_id, trace_id, specversion, time, subject, actor, dataschema.
- [ ] SNS MessageAttributes: EventType, TenantID, Source per platform-events convention.

Consumers (§7.1)

- [ ] internal/adapter/inbound/consumer/membership_event_consumer.go using platform-events.
- [ ] tenant-orgm-q handlers: TrialTenantProvisioned, TenantRealmReady (sets realm_id+realm_type='dedicated'+keycloak_shard atomically), TenantConverted (sets status='active', subscription_started_at — feature_flags untouched T-9), DirectPaidSignup, TrialExpired, TrialReactivated, TenantSuspended, TenantOffboarded.
- [ ] billing-orgm-q handlers: TenantPlanChanged (T-9), TenantSubscriptionCancelled (status+cancelled_at in lockstep T-11), TenantReactivated (only if not offboarded, PAID-1), TenantSeatsChanged (unconditional accept SEAT-2, may open/close overage).
- [ ] EVT-14 recency guard: every tenant-lifecycle handler UPDATE ... WHERE id = $1 AND record_version = $2 AND (last_event_at IS NULL OR $event_time > last_event_at) under row lock. On no-op: skip state, still record processed_events. Metric iam_stale_lifecycle_event_skipped_total.
- [ ] EVT-15 clamp: if event.time > now() + MAX_LIFECYCLE_EVENT_SKEW_SECONDS (default 300), reject to DLQ, do not record processed_events. Metric iam_future_lifecycle_event_rejected_total.
- [ ] EVT-16 relay: whenever status or plan actually changes (post-EVT-14), enqueue TenantStateChanged on iam.membership.events in same RunInTx. Never on stale-skip or no-op.
- [ ] Seat overage transitions (SEAT-5): every SEAT-1/2/3 mutator sets/clears overage_since inline under the tenant lock. TenantSeatOverageStarted / TenantSeatOverageResolved emitted on transitions only.
- [ ] Idempotency (IDEMP-2/4): every consumer INSERT INTO processed_events(event_id, consumer) ON CONFLICT DO NOTHING short-circuits.
- [ ] DLQ -dlq with maxReceiveCount=5 (EVT-5).

Phase 3 tests:

- Integration: EVT-14 out-of-order sequence (stale skip, last_event_at unchanged); EVT-15 poison-pill DLQ (no processed_events row); EVT-16 relay fires only on real change (not on stale, not on duplicate); cancelled_at lockstep; PAID-1 rejects reactivation of offboarded tenants; SEAT-5 transition emits paired start/resolved events under concurrent mutations.
- Idempotency: replay same event twice → processed_events once.
- Contract: schema-registry CI check on drift.

---
Phase 4 — Internal endpoints + outbound clients (LLD §6, §18)

Goal: I-*, O-*, and the three outbound service clients. The delegation flow, the AuthZ hot path, and the invite-accept branch light up.

Internal routes (I-*)

- [ ] RequireSystemRole middleware for /api/v1/internal/* (IAPI-1..3, AUTH-5).
- [ ] POST /tenants (I-1) — RP/Signup BFF provisioning path; inside one RunInTx: 5 system-dept activations + 3 role labels + owner membership + TenantCreated + TrialStarted outbox events.
- [ ] PATCH /tenants/:id (I-2) — RP sets realm_id+realm_type+shard atomically post-realm-provision.
- [ ] POST /tenants/:id/members (I-3) — Event Consumer path; also invitation-acceptance branch (§8.10, PI-4): match pending invitation by keycloak_user_id OR by email (partial-unique), flip to accepted, create tenant membership + initial_tenant_roles + initial_dept_mappings, emit their events. Idempotent on replay (PI-10).
- [ ] PATCH /tenants/:id/members/:user_id (I-4) — Keycloak sync path.
- [ ] DELETE /tenants/:id/members/:user_id (I-5) — same delegate-impact cascade as P-8 but no RP RevokeUserSessions (user delete already killed sessions); sets ownerless_since if needed.
- [ ] GET /users/:id/memberships (I-8) — HOT PATH. Single joined query over tenant_memberships + tenants + tenant_roles + delegations filtered on active. array_agg(...) FILTER (...) so arrays never null (I8-4). Derived member injected at projection layer via resp.Roles = union(["member"], resp.Roles) (TR-7 / §16 A29). effective_feature_flags = planDefaults ⊕ tenants.feature_flags computed here (PLAN-6). mfa_freshness_seconds comes from om:tenant cache not the frozen memberships snapshot (§16 A52). Cache om:memberships:{tenant}:{user} 300s ± 30s jitter (CACHE-4). SLO: 15ms hit / 30ms miss (§21).
- [ ] GET /tenants/:id/locale (I-9), POST /tenants/:id/group-mappings/jit (I-10 — additive-only GTRM-4), GET /tenants/:id/seat-usage (I-11), GET /tenants/:id/acl/:user_id (I-12 — active grant TAE-3), POST /tenants/:id/assignee-override (I-13 — transient only, 422 assignee_ineligible per §16 A62, no persistence).

Operator routes (O-*)

- [ ] AUTH-6 handler re-check of platform_operator from route before any DB access; AUTH-7 defense-in-depth (network + handler + gateway hygiene).
- [ ] POST /departments (O-1), PATCH /departments/:id (O-2 editable, system dept retire → 422 system_department_cannot_be_retired), DELETE /departments/:id (O-3 → 405).
- [ ] PATCH /tenants/:id/feature-flags (O-4) — full-replacement map (§16 A18); allow-list scalar-only validation (PLAN-6(d)); 400 unknown_feature_flag / invalid_feature_value.
- [ ] GET /plans[/:code] (O-5), PATCH /plans/:code (O-6) — update PLAN-4; evicts om:plans.
- [ ] POST /tenants/:id/reassign-owner (O-7) — recover ownerless tenant; grant tenant_owner to existing active member, clear ownerless_since (§16 A39, TM-12/T-13); 422 invalid_owner_member; 409 tenant_offboarded if terminal.

Outbound clients (§18)

- [ ] internal/adapter/outbound/userprofile/http_client.go — GetAvailability(userID, availability). Body varieties: create{status:"ooo", ooo_from, ooo_until, delegate_id} and end {delegate_id: null} only (pointer-clear, DEL-6, matches UP LLD). USER_PROFILE_TIMEOUT_MS default 3000. gincommon propagation.
- [ ] internal/adapter/outbound/workflow/http_client.go — GetDelegateImpact(ctx, tenantID, userID, delegationID *uuid.UUID), ReassignDelegate, CancelByDelegate. WFI-11 delegation_id scoped. WORKFLOW_TIMEOUT_MS default 3000. WFI-8 → 503 workflow_service_unavailable. WFI-13 suspension advisory fail-open.
- [ ] internal/adapter/outbound/realmprovisioner/http_client.go — CreateInvitedUser, DeleteUser (idempotent, PI-9), PatchRealmConfig(T-15, Option A local-first; on non-200 return 202-equivalent and set realm_sync_pending), RevokeUserSessions (AUTH-8 fail-open with iam_session_revoke_failed_total metric). REALM_PROVISIONER_TIMEOUT_MS default 3000.
- [ ] Each outbound client tags unratified methods (RevokeUserSessions, PatchRealmConfig, CreateInvitedUser, DeleteUser) with a comment referencing the register item (A7/A46/A58) and notes under the prompt's "make each degrade safely."

Delegation flow (§8.6, §8.7)

- [ ] POST /delegations (P-19) — validate delegate is active → UP SetAvailability create → wait 200 → only then RunInTx { INSERT delegations; enqueue DelegationStarted }. If UP 4xx → 422 invalid_delegate (CONS-2). Never emit DelegationStarted without a committed UP update.
- [ ] DELETE /delegations/:id (P-20) — call UP SetAvailability with {delegate_id: null} (pointer-clear only, DEL-6, no unavailable status) → UPDATE delegations SET status='cancelled', ended_reason='cancelled'. Defer (leave active) on UP failure.

Phase 4 tests:

- Contract tests for each outbound client (mock HTTP).
- Integration: I-8 hot-path shape correctness (arrays never null); effective_feature_flags merge; mfa_freshness_seconds from tenant cache; I-8 latency SLO smoke.
- I-13 assignee-override eligibility: 422 assignee_ineligible on wrong level/wrong dept; emits TenderAssigneeOverridden on pass; persists nothing (OVR-1).
- Delegation availability-first ordering: UP failure → no DB row, no outbox row.
- Delegation end pointer-clear semantics.
- O-7 reassign-owner clears ownerless_since and grants tenant_owner.
- AUTH-6 operator route rejects non-platform_operator before DB access.

---
Phase 5 — Reconcilers & CronJobs (LLD §13.1)

Goal: 11 CronJobs, each idempotent and safe under restarts.

One cmd/reconciler/main.go binary dispatching by env/flag, plus Helm CronJob templates. Open question below on whether to prefer that or a per-job binary.

- [ ] invitation-expiry — flip pending → expired where expires_at < now(); frees seat (SEAT-1 recount); may trigger SeatOverageResolved.
- [ ] invitation-kc-cleanup (PI-9) — sweep kc_cleanup_pending=true; call RP DeleteUser (idempotent); clear marker on success.
- [ ] realm-config-sync (T-15) — sweep realm_sync_pending=true tenants; call RP PatchRealmConfig; clear on 200; disabling local_accounts_enabled → false prioritized (security-first).
- [ ] seat-overage-reconcile (SEAT-5) — daily backstop: for each tenant, recompute active + pending; set/clear overage_since under row lock; emit paired start/resolved events on transitions only.
- [ ] delegation-expiry — sweep delegations WHERE ends_at < now() AND status='active': call UP {delegate_id: null} first, then transition to ended + emit DelegationEnded ended_reason='expired'; defer on failure for next cycle (DEL-6 defer).
- [ ] trial-cleanup — post-15-day grace, hard-delete trial_expired tenants (paid never hard-deleted).
- [ ] ownerless-alert-exporter — gauge iam_tenant_ownerless.
- [ ] realm-sync-pending-exporter — gauge for stuck reconciliations.
- [ ] seat-overage-exporter — gauge for tenants over cap.
- [ ] outbox-prune — nightly outboxRunner.PrunePublished (mirrors sibling maintenance-sweep).
- [ ] processed-events-prune — daily prune of rows older than retention (IDEMP-4).

Helm: eleven deploy/helm/templates/*-cronjob.yaml files, schedules, concurrencyPolicy: Forbid, restartPolicy: Never, env from the same Secret.

Phase 5 tests:

- Integration: convergence tests — set kc_cleanup_pending/realm_sync_pending/overage_since, run the reconciler, verify expected side-effects (RP call, event emitted).
- Restart-safe: partial run → replay → same final state (IDEMP-1).

---
Phase 6 — Observability & hardening (LLD §11, §16 A48)

Goal: The iam_* metrics + labels the LLD names, alerts, traces.

- [ ] internal/adapter/outbound/metrics/business.go —
  - Counters: iam_stale_lifecycle_event_skipped_total, iam_future_lifecycle_event_rejected_total, iam_session_revoke_failed_total, iam_rls_violations_total, iam_seat_limit_reached_total, iam_workflow_resolution_required_total, iam_invite_rate_limited_total.
  - Gauges: iam_tenant_ownerless (T-13), iam_realm_sync_pending, iam_seat_overage_active, iam_pending_invitations_stale.
  - Histograms: iam_authz_membership_lookup_seconds (I-8), iam_workflow_delegate_check_seconds (WFI SLO).
- [ ] Cardinality guardrails (§16 A48) — no user_id/tenant_id on high-frequency counters unless bucketed; job label for exporter dashboards.
- [ ] /metrics scraped via ServiceMonitor template.
- [ ] deploy/monitoring/app-alerts.yml — burn-rate rules (SLO).
- [ ] Structured logs (zap): tenant_id redacted per production config; trace_id, request_id, event_id on relevant lines.
- [ ] OTel spans on outbound HTTP calls, I-8 query, delegation flow.
- [ ] HPA 2–8 in deploy/helm/templates/hpa.yaml.

---
Environment variables to catalogue in .env-example (§12)

APP_ENV, DATABASE_URL, MIGRATION_DATABASE_URL, SYSTEM_DATABASE_URL, PG_MAX_CONNS, PG_MIN_CONNS, PG_SLOW_QUERY_MS, VALKEY_URL, AWS_REGION, AWS_ENDPOINT_URL, SNS_TOPIC_MEMBERSHIP_ARN, SNS_TOPIC_TENANT_ARN, SQS_TENANT_ORGM_QUEUE_URL, SQS_BILLING_ORGM_QUEUE_URL, GLUE_REGISTRY_NAME, WORKFLOW_SERVICE_BASE_URL, WORKFLOW_TIMEOUT_MS=3000, USER_PROFILE_SERVICE_BASE_URL, USER_PROFILE_TIMEOUT_MS=3000, REALM_PROVISIONER_BASE_URL, REALM_PROVISIONER_TIMEOUT_MS=3000, INVITATION_EXPIRY_DAYS=7, SEAT_OVERAGE_GRACE_DAYS=30, MAX_LIFECYCLE_EVENT_SKEW_SECONDS=300, INVITE_RECOOLDOWN_MINUTES, INVITE_MAX_PER_TENANT_PER_HOUR, OUTBOX_POLL_INTERVAL, OUTBOX_BATCH_SIZE, OUTBOX_MAX_ATTEMPTS, OUTBOX_DRAIN_TIMEOUT, OUTBOX_PUBLISH_CONCURRENCY, OUTBOX_PUBLISH_TIMEOUT, OUTBOX_STARTUP_JITTER, OUTBOX_CLAIM_LEASE_DURATION, OTEL_EXPORTER_OTLP_ENDPOINT, DOCS_AUTH_TOKEN.
































