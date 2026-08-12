# CLAUDE.md

This file provides guidance to Claude Code when working with the **Org & Membership Service** (`iam-org-membership`), a Go microservice in the IAM subsystem (Tender Management SaaS platform). Refines **IAM HLD v1.41 §5.6**; LLD v1.73 (Draft), owner database `org_membership` on shared RDS PostgreSQL. Where LLD and HLD disagree, HLD is authoritative.

## What This Repo Is

`iam-org-membership` is a **private Go service** (module: `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership`) that owns the **organizational layer** of the IAM platform: how users are grouped into tenants and departments, what roles they hold, and how tenants are structured and entitled. Keycloak owns authentication; User Profile owns presentation identity; this service owns the business domain model. It is the source of truth consumed by **AuthZ Enrichment** via `GET /api/v1/internal/users/:id/memberships` (I-8) — the hottest path in the system, hit on every authenticated request.

**Key responsibilities (sole writer for):**
- **Tenants** — plan, subscription status, trial metadata, realm identity/type/shard, MFA freshness, locale, `local_accounts_enabled`, licensed seats, ownerless/overage/realm-sync markers; `delegation_max_duration_days`/`delegation_review_window_days` (tenant-configurable, §16 A71, DEL-14)
- **Plans catalog** (`plans`, §16 A19) — global operator-editable per-tier entitlements (workflow/tender limits, SSO, branding, `feature_set`, `trial_duration_days`); `tenants.plan` FK
- **Departments** — global operator catalog (`is_system`, `is_active`, never physically deleted) and per-tenant activation (`tenant_departments`)
- **Tenant memberships** (`tenant_memberships`) — lifecycle only, no role data (§16 A14)
- **Tenant-level role grants** (`tenant_roles`) — `tenant_owner`/`tenant_admin`/`tender_admin` only; `member` **derived at read time**, never persisted (TR-7, §16 A29)
- **Department memberships** (`dept_memberships`) — user↔department↔role-level (`preparator`/`reviewer`/`approver`); + `dept_role_labels` (tenant-customizable display labels)
- **Group→role/department mappings** — three distinct tables: `group_dept_role_mappings` (path `/group-mappings/department-roles`, renamed §16 A64), `group_tenant_role_mappings` (path `/group-mappings/tenant-roles`, §16 A25), `group_dept_mappings` (path `/group-mappings/departments`)
- **Delegation grants** (`delegations`) — authoritative record for workflow rerouting (scope: all/department/tender; hard time bounds capped by tenant's `delegation_max_duration_days`, DEL-14); open-ended delegations (`ends_at IS NULL`) carry a rolling `review_due_at` window (DEL-13, §16 A70/A71) enforced by the `delegation-review` CronJob; `review_due_at`, `review_notice_sent_at`, `review_window_days` columns
- **Tender ACL overlays** (`tender_acl_entries`) — additive view/edit/approve grants (§16 A32(c) HLD-aligned)
- **Pending invitations** (`pending_invitations`, new §16 A11) — two-step invite→accept staging (**one PII exception**: email + full_name until acceptance)
- **Plan tier / feature-flag overrides** — `tenants.plan` (FK), `tenants.feature_flags` (per-tenant delta only, T-9); effective set = `planDefaults(plan) ∪ feature_flags` at read time

**Does NOT own:** credentials, MFA enforcement, JWT issuance (Keycloak); display identity, signature, OOO presentation flag (User Profile); realm/Keycloak Admin API mutations (Realm Provisioner — this service **never** calls Keycloak Admin API); workflow template authoring or `assignee_overrides` state (Workflow Service — we validate + emit `TenderAssigneeOverridden` via I-13, never persist; §16 A32(d)/OVR-1); audit records (Audit Log); tender content/bids (Tender Service); metered resource consumption / quota enforcement (Usage & Metering — HLD §10.6 "IAM does not count tokens or requests itself"); pricing / currency (Billing — `default_currency` deliberately not stored here, §16 A32(b)/T-3).

**Ownership split with User Profile — delegation:** `delegations` (here) is authoritative for workflow rerouting. `user_availability` (User Profile) is presentation-only. Coordination pattern (§8.6, CONS-2): validate delegate → call User Profile's `PUT /internal/users/:id/availability`, wait for 200 → only then insert `delegations` + outbox event in one `RunInTx`. `DelegationStarted` is never enqueued without a committed User Profile update. Expiry (§8.7, DEL-6): pointer-clear only (`{delegate_id: null}`), never `{status: available}` — the delegator's return is UP-owned.

## Common Commands

```bash
make setup            # Copy .env-example → .env (run once before anything else)
make tidy              # go mod tidy
make fmt               # go fmt ./...
make fmt-check         # Verify gofmt formatting without modifying files (mirrors CI)
make vet               # go vet ./...
make lint              # golangci-lint (via go tool golangci-lint)
make mod-verify        # go mod verify (check module download integrity)
make vuln-check        # govulncheck ./internal/...
make test              # All tests (unit + postgres + integration + e2e, requires Docker)
make test-ci           # Unit + postgres + integration with -race + coverage (used in CI)
make test-unit         # Unit tests only (no Docker required)
make test-postgres     # Postgres + RLS integration tests (via testcontainers-go)
make test-integration  # Cross-layer integration tests (SNS/SQS via LocalStack, testcontainers)
make test-e2e          # End-to-end tests
make test-smoke        # Staging smoke path: provision → member add → dept assign → delegation → expiry
make race              # All tests with -race flag
make run               # Run server locally (sources .env, kills port 8080 first)
make build             # Compile to bin/iam-org-membership
make cover             # Coverage HTML report (measures ./internal/...)
make cover-func        # Coverage summary by function (terminal)
make ci                # tidy + vet + lint + test-ci + build (full CI pipeline)
make schema-verify     # Pre-deploy check: Glue registry schema names/versions vs api/asyncapi.yaml + internal/eventschema/*.json (via schema-gov)
make docker-up         # Start PostgreSQL + PgBouncer + Valkey + LocalStack (Docker required)
make docker-down       # Stop containers
make clean             # Remove bin/ artefacts and coverage files
```

To run a single test:
```bash
go test ./test/unit/membership_service/... -run TestRemoveUserDelegateImpact -v
go test ./test/postgres/... -run TestRLSPolicyEnforcement -v
```

**Testcontainers note:** Postgres, Valkey, and SNS/SQS (LocalStack) integration tests spin up real containers via `testcontainers-go`. Docker must be running. Pass `-short` to skip integration tests without Docker.

**`platform-schemagov` note:** `schema-gov` is a Python 3.12 CLI, **not a Go module** — invoked via `docker run ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4` in CI only (`.github/workflows/schema-registry.yml`: extract → validate → enforce-lifecycle → diff on PRs; register/changelog/metrics on `main`). Workspace is `api/asyncapi.yaml` + `internal/adapter/outbound/eventbus/schemas/*.json` (schemas colocated with the eventbus adapter, embedded via `//go:embed schemas/*.json` in the ValidatingCodec). Pinned to `0.4` in Phase 0 to match sibling `iam-user-profile2`.

## Architecture

Clean Architecture — dependencies point inward; outer layers never import inner layers. Full directory tree, shared library dependencies (`platform-gincommon` v1.2.0, `platform-events` v1.4.0, `platform-pgcommon` v1.1.1), `RoutingPublisher` two-topic wiring, and `go-arch-lint` dependency rules are in **[`.claude/architecture.md`](architecture.md)**.

## Key Files to Know

- **`cmd/server/main.go`** — wiring, pool setup (`pgcommon.NewPool` with `GUCProvider = GUCSetFromContext` → transaction-local `app.tenant_id` binding on every checkout, RLS-6), middleware, graceful shutdown, plus 4 in-process metric-exporter goroutines (`iam_tenant_ownerless`, `iam_realm_sync_pending`, `iam_seat_overage_active`, `iam_pending_invitations_stale`) started alongside the outbox runner. K8s CronJobs count is **9** (write-side reconcilers dispatched by `cmd/reconciler/main.go --job=<name>`, Option A single binary) — exporters are goroutines, not CronJobs. See `operations.md`.
- **`internal/core/domain/*.go`** — entities, value objects, `DomainError` catalogue (`ErrWorkflowResolutionRequired`, `ErrSeatLimitReached`, `ErrInvitationAlreadyExists`, `ErrInvalidReplacement`, `ErrConflict`). No external deps.
- **`internal/core/service/membership_service.go`** — most complex service. `RemoveUser` synchronously calls `port.WorkflowClient.GetDelegateImpact` before removal cascade; on `active_workflows > 0` returns `409 workflow_resolution_required`, resolved via `POST /tenants/:id/users/:user_id/removal-resolution` (P-26, `replace_delegate`/`stop_workflows`). Removal cascade also soft-deletes `tenant_roles` in the same `RunInTx` (step 1b, TR-9). Also handles department-level demotion/removal gating (§8.8.4, scoped by `delegation_id`), suspension advisory (§8.8.5, fail-open WFI-13), invitation lifecycle (§8.10 — `Invite`, `AcceptInvitation` via I-3), SEAT-1 transactional cap enforcement, TM-8 last-owner protection + TM-13 concurrency serialization (owner-affecting mutations take `SELECT … FOR UPDATE` on `tenants` row), TM-12 identity-layer escalation (`ownerless_since`).
- **`internal/core/service/delegation_service.go`** — OOO coordination: validate delegate is active member → pre-flight rejects past `starts_at` (`422 delegation_start_in_past`, §16 A65) and fixed-end spans exceeding `delegation_max_duration_days` (`422 delegation_window_too_long`, §16 A71) → call User Profile `SetAvailability` and wait for `200` (also handles new UP `422 delegate_unavailable`, §16 A66) → only then insert `delegations` + enqueue `DelegationStarted` in one `RunInTx`. **Delegation-review** (§8.7.1, DEL-13, §16 A70): `delegation-review` CronJob sweeps open-ended delegations via `idx_delegations_review_due`, emits `DelegationReviewRequested` at 7 d/3 d before `review_due_at`, auto-ends at `review_due_at` if untouched. **P-32** `POST …/extend` pushes `review_due_at` forward (rejects `extend_days` outside `[1, 180]`, `422 extend_days_out_of_range`); **P-33** `POST …/reassign` ends + creates a fresh delegation. Expiry cron clears the UP delegate pointer before ending the delegation row; defers (leaves `active`) on UP failure (DEL-6). DEL-14: both `delegation_max_duration_days` and `delegation_review_window_days` are bounded `[1, 180]`; `starts_at` additionally capped at 1 year out.
- **`internal/core/port/workflow_client.go`** — newest port (§8.8.1). `GetDelegateImpact`/`ReassignDelegate`/`CancelByDelegate`; all take optional `delegation_id *uuid.UUID` for department-scoped queries (WFI-11). No inbound subscription from Workflow.
- **`internal/core/port/user_profile_client.go`** — `SetAvailability` for delegation coordination flow.
- **`internal/core/port/realm_provisioner_client.go`** — `CreateInvitedUser`, `DeleteUser` (idempotent, PI-9), `PatchRealmConfig` (T-15, Option A local-first + reconcile), `RevokeUserSessions` (AUTH-8, best-effort fail-open).
- **`internal/adapter/inbound/http/membership_handler.go`** — `DELETE` pre-check; `POST .../removal-resolution` (P-26); invite lifecycle (P-6/P-30/P-31); seat-usage (P-27).
- **`internal/adapter/inbound/http/internal_handler.go`** — I-12 `GET …/acl/:user_id` (tender ACL check, §16 A51); I-13 `POST …/assignee-override` (validate-and-emit `TenderAssigneeOverridden`, never persists, OVR-1, §16 A55); I-14 `GET …/mfa-freshness` (returns `mfa_freshness_seconds` from `om:tenant` cache — authoritative read path for AuthZ Enrichment step-up gate, §16 A72; modeled on I-9); I-8 now projects `subscription_status` + derived `read_only` (§16 A53).
- **`internal/adapter/inbound/consumer/membership_event_consumer.go`** — SQS handlers for `tenant-orgm-q`/`billing-orgm-q`. **EVT-14** recency guard (event `time` vs `tenants.last_event_at` under row lock — silently skips stale, still records `processed_events`); **EVT-15** future-time clamp (`event.time > now() + MAX_LIFECYCLE_EVENT_SKEW_SECONDS` → DLQ, not recorded); **EVT-16** tenant-state relay (re-emits `TenantStateChanged` on `iam.membership.events` in the same tx when a consumed event changes `status`/`plan`, sparing Workflow a direct `iam.tenant.events`/`billing.events` subscription).
- **`internal/adapter/outbound/postgres/*_repository.go`** — implements all `core/port` repositories; all 14 `record_version`-carrying tables use `UPDATE ... WHERE id=$1 AND record_version=$2` optimistic locking (409 `optimistic_lock_conflict`, TRG-1 client never sets `record_version`).
- **`internal/adapter/outbound/eventbus/publisher.go`** — `events.NewRoutingPublisher` (two topics by `Envelope.Source`) + outbox runner; UUID v7 event IDs; `processed_events` composite PK 8-day retention (PE-1 strictly > 7-day SQS lifetime; IDEMP-4 beyond-window duplicates backstopped by EVT-14/PI-10/IDEMP-3). All published events carry `ip_address`/`user_agent` envelope fields (`platform-events` v1.4.0 `WithIPAddress`/`WithUserAgent`, §16 A69); CronJob-originated events use sentinel `ip_address: "system"` / `user_agent: "iam-org-membership/<job-name>-cron"`.
- **`internal/adapter/outbound/{userprofile,workflow,realmprovisioner}/http_client.go`** — three outbound service clients, identical construction pattern (`gincommon.PropagateHeaders`, configurable `*_TIMEOUT_MS` env var, default 3000ms).
- **`internal/eventschema/*.json`** — schema-governance workspace, one JSON Schema Draft-07 file per event type (10 files at present including `TenantRoleRevoked` for multi-role §16 A14). Derived via `schema-gov extract`; CI `extract --check` for drift.
- **`api/asyncapi.yaml`** — design-time source of truth for two-topic event contract (`iam.membership.events` + `iam.tenant.events`).
- **`api/openapi.yaml`** — REST contract for all public/internal/operator routes (P-1..P-33, I-1..I-14, O-1..O-7).
- **`test/postgres/rls_test.go`** — canonical RLS test cases including **Case 5** (critical): no cross-tenant GUC leak across a pooled PgBouncer connection (verifies RLS-6). CI additionally greps for non-`LOCAL` `SET app.tenant_id` as forbidden pattern.

## Data Model

Database `org_membership` on RDS PostgreSQL (Multi-AZ, PgBouncer transaction pooling); **15 tables** with RLS on all tenant-scoped tables (12 tables, `FORCE ROW LEVEL SECURITY` + `REVOKE ALL FROM PUBLIC` + `tenant_isolation` policy on `app.tenant_id`), `record_version`-based optimistic locking on **14 tables** (all except `processed_events`), and partial unique indexes (`WHERE deleted_at IS NULL` / `WHERE status='pending'`) so a user can rejoin a tenant/department previously left. Full table catalogue, RLS invariants (RLS-1..RLS-6), tenant invariants (T-1..T-15), seat invariants (SEAT-1..SEAT-5), migration invariants (MIG-1..MIG-9), and trigger behavior (`touch_row`, `trg_tenant_slug_immutable`) in **[`.claude/database-schema.md`](database-schema.md)**.

## API, Caching & Events

Three route prefixes (`/api/v1/*`, `/api/v1/internal/*`, `/api/v1/operator/*`); hot-path lookup at `GET /internal/users/:id/memberships` (I-8, SLO 15 ms hit / 30 ms miss, now includes `subscription_status`/`read_only`); public P-1..P-33 (P-32 delegation-extend, P-33 delegation-reassign), internal I-1..I-14 (I-12 tender-ACL check, I-13 assignee-override, I-14 mfa-freshness), operator O-1..O-7. Valkey cache advisory-only (CACHE-2/CACHE-9). Two SNS outbound topics (`iam.membership.events` with `TenantStateChanged` relay §16 A61; `iam.tenant.events` with only `TenantCreated`/`TrialStarted`) via `RoutingPublisher`. Events include `DelegationReviewRequested` (new, §16 A70 — notifies delegator, delegate, all `tenant_admin`/`tenant_owner`) and `TenantSeatOverageStarted`/`TenantSeatOverageResolved` (§16 A59). All published events carry `ip_address`/`user_agent` envelope fields (§16 A69). Two inbound SQS queues (`tenant-orgm-q`, `billing-orgm-q`) with EVT-14 recency guard + EVT-15 future-time clamp + EVT-16 relay. CloudEvents envelope, `platform-schemagov` governance. Full endpoint catalogue, cache TTLs + invalidation, event lists + payloads, and status-code table in **[`.claude/api-caching-events.md`](api-caching-events.md)**.

## Request Flows & Concurrency

Trial signup (§8.1: 5 default system depts — Engineering, Design, Procurement, Finance, Legal — activated over the global operator catalog + 3 role labels + owner + role grant + 2 events in one tx, all inside the `TrialTenantProvisioned` consumer, not I-1); AuthZ hot-path lookup (§8.3); JIT SAML (§8.5, additive-only GTRM-4); OOO delegation with User Profile coordination (§8.6/§8.7 pointer-clear semantics); delegation extend/reassign (§8.7.1, P-32/P-33, DEL-13/DEL-14 — `delegation-review` CronJob auto-ends open-ended delegations past `review_due_at`); user removal with delegate-impact resolution (§8.8: pre-check, P-26 `replace_delegate`/`stop_workflows`, `tenant_roles` soft-deleted in cascade TR-9, department-level extension §8.8.4 with `delegation_id` scoping, suspension advisory §8.8.5 fail-open); two-step invite→accept (§8.10, seat-hold on stage, `kc_cleanup_pending` durable compensation PI-9); optimistic locking (CONC-1..4); idempotency (IDEMP-1..4 with EVT-14/PI-10/IDEMP-3 as beyond-window backstop); consistency (CONS-1..4 write+event atomic via transactional outbox, availability-first delegation); GDPR (§15: soft-delete pattern, User Profile PII scrub, `pending_invitations` erasure-by-email is the sole non-`user_id`-keyed path). Full flow diagrams and invariants in **[`.claude/request-flows.md`](request-flows.md)**.

## Operations

Security (three-layer isolation, AUTH-8 privilege-reduction with RP session revoke + TTL backstop, AUTH-7 defense-in-depth for operator routes); observability (SLO-1..3, `iam_`-prefixed metrics with `job` label for subsystem-wide dashboards, cardinality guardrails §16 A48, alert routing; new delegation-review metrics `iam_delegation_review_pending_total`/`iam_delegation_review_expired_total`); configuration (`DATABASE_URL`, `WORKFLOW_SERVICE_BASE_URL`, `INVITATION_EXPIRY_DAYS=7` coupled to KC action-token lifespan, `SEAT_OVERAGE_GRACE_DAYS=30`, `MAX_LIFECYCLE_EVENT_SKEW_SECONDS=300`; `DELEGATION_REVIEW_WINDOW_DAYS` env var **superseded** by per-tenant `delegation_review_window_days` column, §16 A71); deployment (HPA 2–8, **9 CronJobs** dispatched via `cmd/reconciler --job=<name>`: `invitation-expiry`/`invitation-kc-cleanup`/`realm-config-sync`/`seat-overage-reconcile`/`delegation-expiry`/`delegation-review`/`trial-cleanup`/`outbox-prune`/`processed-events-prune`. The four metric exporters run as ticker goroutines inside `cmd/server/main.go` per the sibling `iam-user-profile2` pattern — not as CronJobs. Additive-then-destructive migrations MIG-1); testing (unit mocks including AUTH-8 fake `RealmProvisionerClient`, integration testcontainers covering EVT-14/15/16 + composite FKs + TM-12/TM-13 + T-15 reconciler, contract tests, e2e/smoke, RLS Case 5); §20.7 fail-open vs fail-closed dependency matrix; §21 performance. Full detail in **[`.claude/operations.md`](operations.md)**.

## See Also

Detailed reference docs in `.claude/`:
- [`architecture.md`](architecture.md) — Clean Architecture directory tree, shared library integration, dep rules
- [`database-schema.md`](database-schema.md) — 15 tables, enums, RLS/tenant/seat/migration/trigger invariants
- [`api-caching-events.md`](api-caching-events.md) — full endpoint catalogue (P-*/I-*/O-*), authz invariants, cache keys, event catalogue, EVT-14/15/16
- [`request-flows.md`](request-flows.md) — provisioning, delegation, delegate-impact resolution (§8.8), invite→accept (§8.10), concurrency, GDPR
- [`operations.md`](operations.md) — security, observability, configuration, CI/CD, integration points, dependency degradation matrix

Full LLD (v1.71 Draft, 4998 lines): `/Users/sharmila/bcbp-solutions/Documents/LLD/Org-Membership/Documents/org_membership_lld.md` — §16 open-question register, §17 error taxonomy, §19 migration strategy.

Supplementary docs in the repo root:
- **`README.md`** — onboarding, prerequisites, quick-start, local dev setup
- **`ARCHITECTURE.md`** — detailed architecture narrative with diagrams
