# CLAUDE.md

This file provides guidance to Claude Code when working with the **Org & Membership (Core) Service** (`iam-org-membership`), a Go microservice in the IAM subsystem (Tender Management SaaS platform). Refines **IAM HLD v1.41 §5.6**; LLD v2.44 ("Org & Membership (Core) Service — Low-Level Design"), owner database `org_membership` on shared RDS PostgreSQL. Where LLD and HLD disagree, HLD is authoritative.

This repo is the end-state of a completed **service decomposition** (ADR-0007 + ADR-0008): departments/plans catalog ownership moved to **Catalog / Admin Config Service** (`iam-catalog-admin`), SAML group→dept/role mapping moved to **Group Mapping / JIT Config Service** (`iam-group-mapping`), tender ACL overlays moved to **Tender ACL Service** (`iam-tender-acl`), and delegation grants + OOO coordination moved to **Delegation Service** (`iam-delegation`). This repo — "Core" — is what's left after all four extractions; the removal has already landed on this branch (pre-production, no staged expand/contract needed).

## What This Repo Is

`iam-org-membership` is a **private Go service** (module: `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership`) that owns the **organizational layer** of the IAM platform: how users are grouped into tenants and departments, what roles they hold, and how tenants are structured and entitled. Keycloak owns authentication; User Profile owns presentation identity; this service owns the business domain model. It is the source of truth consumed by **AuthZ Enrichment** via `GET /api/v1/internal/users/:id/memberships` (I-8) — the hottest path in the system, hit on every authenticated request.

**Key responsibilities (sole writer for):**
- **Tenants** — plan (ENUM, not FK), subscription status, trial metadata, realm identity/type/shard, MFA freshness, locale, `local_accounts_enabled`, licensed seats, ownerless/overage/realm-sync markers, `suspension_source` (`billing_lapse`|`operator`, new T-16 — distinguishes an RP-14 administrative suspension, which can land directly from `active`/`trial` and skips the billing grace clock, from the normal cancelled→suspended lapse). No longer carries `delegation_max_duration_days`/`delegation_review_window_days` (moved to Delegation Service's own `delegation_tenant_settings`).
- **Department activation** (`tenant_departments`) — per-tenant activation junction only; the global department catalog itself lives in Catalog Service, validated app-side against an `om:departments` read-through cache, not a local FK.
- **Tenant memberships** (`tenant_memberships`) — lifecycle only, no role data (§16 A14)
- **Tenant-level role grants** (`tenant_roles`) — `tenant_owner`/`tenant_admin`/`tender_admin` only; `member` **derived at read time**, never persisted (TR-7, §16 A29)
- **Department memberships** (`dept_memberships`) — user↔department↔role-level (`preparator`/`reviewer`/`approver`); + `dept_role_labels` (tenant-customizable display labels, presentation only — no group→role mapping data, that moved to Group Mapping Service)
- **Pending invitations** (`pending_invitations`, new §16 A11) — two-step invite→accept staging (**one PII exception**: email + full_name until acceptance)
- **Plan tier / feature-flag overrides** — `tenants.plan` (ENUM) and `tenants.feature_flags` (per-tenant delta only, T-9); effective set = `planDefaults(plan) ∪ feature_flags` at read time, `planDefaults` now sourced from Catalog Service via `om:plans` read-through cache instead of a local `plans` table

**Does NOT own:** credentials, MFA enforcement, JWT issuance (Keycloak); display identity, signature, OOO presentation flag (User Profile); global department catalog / plan entitlement catalog (Catalog / Admin Config Service); group→dept/role JIT mapping tables (Group Mapping / JIT Config Service); tender ACL overlay `tender_acl_entries` (Tender ACL Service); delegation record/lifecycle/policy/events `delegations` (Delegation Service); realm/Keycloak Admin API mutations (Realm Provisioner — this service **never** calls Keycloak Admin API); workflow template authoring or `assignee_overrides` state (Workflow Service — we validate + emit `TenderAssigneeOverridden` via I-13, never persist; §16 A32(d)/OVR-1); audit records (Audit Log); tender content/bids (Tender Service); metered resource consumption / quota enforcement (Usage & Metering — HLD §10.6 "IAM does not count tokens or requests itself"); pricing / currency (Billing — `default_currency` deliberately not stored here, §16 A32(b)/T-3).

**Core's only remaining delegation touchpoint:** a synchronous delegate-impact gate on user removal (§8.8, via `port.WorkflowClient` — talks to the *Workflow* Service, unaffected by the decomposition) plus a synchronous department-scope precision lookup on the admin dept-membership Assign/Remove path via `port.DelegationCheckClient` → `GET {DELEGATION_BASE_URL}/internal/delegations/dept-delegate`. On a Delegation Service outage, a removal (P-11) degrades to tenant-wide impact scoping (still correct, less precise) and a demotion (P-10) proceeds ungated — never a hard failure. There is no `port.UserProfileClient`/`userprofile` adapter in this repo any more — the old `SetAvailability` OOO-coordination flow was dead code once delegation moved out and has been deleted (LLD §16 OQ-5).

## Common Commands

```bash
make setup            # Copy .env-example → .env (run once before anything else)
make tidy              # go mod tidy
make fmt               # go fmt ./...
make fmt-check         # Verify gofmt formatting without modifying files (mirrors CI)
make vet               # go vet ./... — default build + a second pass with every test build tag (integration,e2e)
make lint              # golangci-lint (via go tool) — default build + every test build tag
make arch-lint         # go-arch-lint against .go-arch-lint.yml (same script CI runs)
make helm-check        # helm lint + template (base/dev/staging/prod) + kubeconform (same script CI runs)
make helm-access-check # CD-19 access matrix: parse rendered staging+prod Istio AuthorizationPolicy + NetworkPolicy, assert per-caller routes (test/helm, tag helm)
make metrics-lint      # platform-gincommon metricslint on a real /metrics scrape + alert/dashboard refs + inventory drift (same script CI runs)
make metrics-inventory # regenerate docs/observability/metric-registry.md (commit it with any metric change)
make mod-verify        # go mod verify (check module download integrity)
make vuln-check        # govulncheck ./internal/...
make test              # Unit + postgres + integration tests, run in parallel (requires Docker; e2e is separate — make test-e2e)
make test-ci           # Unit + postgres + integration with -race + coverage (used in CI)
make test-unit         # Unit tests only (no Docker required)
make test-postgres     # Postgres + RLS integration tests (via testcontainers-go)
make test-integration  # Cross-layer integration tests (SNS/SQS/Glue via floci, testcontainers)
make test-e2e          # End-to-end tests
make test-smoke        # CI-only Docker image gate: image size ≤200MB + startup-gate check (not a functional test)
make race              # All tests with -race flag
make run               # Run server locally (sources .env, kills port 8080 first)
make build             # Compile bin/iam-org-membership (server), bin/reconciler and bin/migrate, then verify library packages compile
make cover             # Coverage HTML report (measures ./internal/...)
make cover-func        # Coverage summary by function (terminal)
make ci                # tidy + fmt-check + vet + lint + arch-lint + test-ci + build (full CI pipeline)
make schema-verify     # Pre-deploy check: every produced schema's definition is registered + AVAILABLE in its registry (get-schema-by-definition — the pod's own startup lookup)
make docker-up         # Start PostgreSQL + PgBouncer + Valkey + floci + floci-ui (web console, http://localhost:4500) — Docker required
make docker-down       # Stop containers
make clean             # Remove bin/ artefacts and coverage files
```

To run a single test:
```bash
go test ./test/unit/... -run TestMembership_RemovalResolution_ReplaceDelegate_HappyPath -v
go test ./test/postgres/... -run TestRLS_Case5_NoCrossTenantLeakAcrossPool -v
```

**Testcontainers note:** Postgres, Valkey, and SNS/SQS/Glue (floci — open-source, drop-in LocalStack replacement, Glue Schema Registry included free) integration tests spin up real containers via `testcontainers-go`. Docker must be running. Pass `-short` to skip integration tests without Docker.

**`platform-schemagov` note:** Python 3.12 CLI, **not a Go module** — installed from source in CI; `ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4` image used only by local `make` targets. Workspace is `api/asyncapi.yaml` + `internal/adapter/outbound/eventbus/schemas/*.json`. **Known `platform-schemagov:0.4` quirk:** `extract` and `validate` derive *different* expected filenames for `MFAReset` from the same `asyncapi.yaml` — no filename satisfies both. Decision: keep `mfa_reset.json` (validate/Pass-7 stays clean); `validate-test.yml`'s "Event schema sync check" treats the exact `{mfareset.json}` drift as a non-blocking known exception. All 14 produced schemas are `additionalProperties: true` (OPEN — org-wide decision 2026-09-21, LLD rev 2.11; all 8 validate passes succeed cleanly).

## Architecture

Clean Architecture — dependencies point inward; outer layers never import inner layers. Full directory tree, shared library dependencies (`platform-gincommon/v2` v2.1.0, `platform-events/v2` v2.2.1, `platform-pgcommon/v2` v2.0.2), `RoutingPublisher` two-topic wiring, and `go-arch-lint` dependency rules are in **[`.claude/architecture.md`](architecture.md)**.

## Key Files to Know

Most-touched entrypoints — full per-file descriptions with port contracts, adapter caveats, and invariant references in **[`.claude/key-files.md`](key-files.md)**:

- `cmd/server/main.go` — wiring, RLS GUC pool setup, 6 in-process metric-exporter goroutines, graceful shutdown
- `internal/core/service/membership_service.go` — most complex; `RemoveUser` delegate-impact gate (§8.8), invite lifecycle (§8.10), SEAT-1 cap, TM-8/TM-13 concurrency serialization
- `internal/core/service/authz_service.go` — I-8 composition (TR-7 derived-role injection, PLAN-6 feature-flag merge); delegates DB join to `port.AuthZRepository`
- `internal/adapter/inbound/http/membership_handler.go` / `internal_handler.go` — HTTP surface (P-*/I-*/O-* routes)
- `internal/adapter/outbound/eventbus/` — `GlueCodec` (startup schema resolution), `RoutingPublisher` (two-topic SNS routing), outbox
- `internal/adapter/inbound/consumer/membership_event_consumer.go` — SQS handlers for `tenant-orgm-q`/`billing-orgm-q`; EVT-14 recency guard, EVT-15 future-time clamp, EVT-16 state relay
- `pkg/requestctx/guc.go` — `WithSystemTenant`/`WithTenant`, load-bearing RLS GUC helpers for every internal route and consumer

## Data Model

Database `org_membership` on RDS PostgreSQL (Multi-AZ, PgBouncer transaction pooling); **10 tables** (`tenants`, `tenant_departments`, `tenant_memberships`, `tenant_roles`, `dept_memberships`, `dept_role_labels`, `pending_invitations`, `processed_events`, `pending_tenant_events`, `trial_signup_ledger` (CR-4: cross-tenant, no RLS; HMAC of trial owners' emails with the claiming `tenant_id`, pruned after 730 days) — `departments`/`plans`/group-mapping tables/`tender_acl_entries`/`delegations` moved to the Catalog, Group Mapping, Tender ACL, and Delegation services respectively — the current schema is one consolidated base migration, `000000_initial_schema`, since this service has never been deployed and the incremental decomposition history had no live data to protect, plus four additive ones: `000001_check_constraints` (length CHECKs on `pending_invitations.full_name`/`dept_role_labels.display_name`), `000002_pub1_pending_invitations_email_idx` (`idx_pi_email_pending` for PUB-1), and `000003_cd37_residency_mode` (`tenants.residency_mode TEXT NOT NULL DEFAULT 'standard' CHECK IN ('standard','strict')`, CD-37), `000004_cd50_pending_tenant_events` (EVT-18 parked events, CD-50), `000005_cr4_trial_signup_ledger` and `000006_cr4_trial_ledger_hardening` (CR-4 grants, `tenant_id` claim column, hash-format CHECK — LLD rev 2.44); `processed_events.event_id` is `uuid`, the shape platform-events' inbox expects) with RLS on all 8 tenant-scoped tables (`FORCE ROW LEVEL SECURITY` + `REVOKE ALL FROM PUBLIC` + `tenant_isolation` policy on `app.tenant_id`), `record_version`-based optimistic locking on 7 tables (all except `processed_events` and `pending_tenant_events`), and partial unique indexes (`WHERE deleted_at IS NULL` / `WHERE status='pending'`) so a user can rejoin a tenant/department previously left. Some cross-database FKs were lost in the split and replaced by synchronous app-level checks (`tenants.plan`/`tenant_departments.department_id`/`dept_memberships.department_id` against `om:plans`/`om:departments`; Tender ACL and Delegation now call Core's new I-15 existence check instead of a composite membership FK). Full table catalogue, RLS invariants (RLS-1..RLS-6), tenant invariants (T-1..T-17), seat invariants (SEAT-1..SEAT-5), migration invariants (MIG-1..MIG-9), and trigger behavior (`touch_row`, `trg_tenant_slug_immutable`) in **[`.claude/database-schema.md`](database-schema.md)**.

## API, Caching & Events

Four route prefixes (`/api/v1/*` — including the caller-self `/api/v1/me/*` — `/api/v1/internal/*`, `/api/v1/operator/*`, `/api/v1/public/*`); hot-path lookup at `GET /internal/users/:id/memberships` (I-8, SLO 15 ms hit / 30 ms miss, four-table join, includes `subscription_status`/`read_only`). **41 active endpoints**: P-1..13,24-28,30-31,34,35 (22 public, including P-35 `GET /api/v1/me/session-context` — the signed-in user's roles/departments/plan/feature_flags/read_only from the I-8 projection plus the tenant's MFA window and locale, for the frontend SDK, LLD rev 2.43), I-1..5,8-11,13-19 (16 internal, including I-15 membership-existence check, I-16 subscription-lapses bulk read, I-17 plan-context for Entitlements (CD-13), I-18 plan-change-check for Billing (CD-20), and I-19 assignee eligibility for the Workflow Services — uncached, path tenant bound as RLS tenant, LLD rev 2.30), O-4,O-7 (2 operator), PUB-1 (`GET /api/v1/public/tenants/resolve` — the one unauthenticated route; its edge carve-out is `ingress.publicResolve`, `templates/publicresolve.yaml`: Exact GET HTTPRoute, non-merging SecurityPolicy, per-IP BackendTrafficPolicy limit, LLD rev 2.43). Retired IDs (P-14..23,29,32,33; I-6,7,12; O-1,2,3,5,6) are fully unregistered, never reused. **Internal tenant resolution (IAPI-6, LLD rev 2.31):** on every `/api/v1/internal/tenants/:id/*` route the **path tenant** is the RLS tenant — `BindPathTenant` (route group only, `path_tenant.go`) rebinds `app.tenant_id` over `x-tenant-id`, logs/counts a mismatch (`iam_org_membership_tenant_header_path_mismatch_total`), and rejects it `400 TENANT_MISMATCH` when `STRICT_TENANT_PATH_VALIDATION` (Helm `iam.strictTenantPathValidation`, default false) is on; register new tenant-scoped internal routes on `tenantScoped` or `TestInternalTenantRoutes_AllBindPathTenant` fails. I-8 binds `?tenant_id=`; public routes keep the header (API-2), enforced centrally by `RequirePathTenantMatch` on the `/tenants/:id` group (403 on mismatch, no rebinding; register new tenant-facing routes on that group or `TestPublicTenantRoutes_AllRequirePathTenantMatch` fails). Valkey cache advisory-only (CACHE-2/CACHE-9); read-through keys `om:plans`/`om:departments` (Catalog Service) and `om:grm`/`om:gdm`/`om:gtrm` (Group Mapping Service). Two SNS outbound topics via `RoutingPublisher`: `iam.membership.events` and `iam.tenant.events`. Three inbound SQS queues: `tenant-orgm-q`, `billing-orgm-q` (EVT-14/15/16, plus EVT-18: an event for a tenant with no row yet is parked in `pending_tenant_events` and acknowledged, then replayed in order by I-1 right after it creates the row, with the `parked-events-replay` CronJob as backstop — CD-50, LLD rev 2.41; `tenant-orgm-q` also carries RP's `TenantIdentityProvisioned`, the **signup provisioning backstop** — no tenant row → the consumer runs I-1 from the payload, LLD rev 2.33), and `catalog-orgm-q` (Catalog's `DepartmentCatalogChanged`/`PlanCatalogChanged`, cache invalidation only; `PlanCatalogChanged` bumps `om:plans:gen:{plan}`, which retires the plan's cached I-8 entries — CACHE-11, LLD rev 2.39; never writes `feature_flags`, T-9). Full endpoint catalogue, cache TTLs + invalidation, event lists + payloads, and status-code table in **[`.claude/api-caching-events.md`](api-caching-events.md)**.

## Request Flows & Concurrency

Trial signup (§8.1: I-1, 5 default depts + owner + role grant + 2 events in one tx); AuthZ hot-path (§8.3); JIT SAML (§8.5, Group Mapping Service call, additive-only GTRM-4); user removal with delegate-impact gate (§8.8: `port.WorkflowClient` pre-check, P-26 resolution, `tenant_roles` cascade TR-9, one `MembershipRevoked` shared by AuthZ Enrichment/Delegation/Tender-ACL); two-step invite→accept (§8.10, SEAT-1 hold, PI-9 durable compensation); trial hard-delete (§8.10.3: `trial-cleanup` CronJob emits `TenantMembershipsPurged` atomically with `HardDeleteTenant` per expired tenant); optimistic locking (CONC-1..4); idempotency (IDEMP-1..4); consistency via transactional outbox (CONS-1..4); GDPR soft-delete + `TenantMembershipsPurged` cascade signal (two paths: paid offboard via consumer, trial hard-delete via `trial-cleanup`). Full flow diagrams and invariants in **[`.claude/request-flows.md`](request-flows.md)**.

## Operations

Security (three-layer isolation, AUTH-8 session revoke + TTL backstop); observability (three-tier metric hierarchy — Tier 1 `platform_*` via `gincommon.NewShared*Vec`, Tier 2 `iam_*` IAM-domain-shared, Tier 3 `iam_org_membership_*` service-unique; `make metrics-lint` enforces compliance; two Tier-1 metrics are `StatusProposed` and emitted alongside authoritative Tier-3 counterparts); **9 CronJobs** via `cmd/reconciler --job=<name>` (`invitation-expiry`, `invitation-kc-cleanup`, `realm-config-sync`, `seat-overage-reconcile`, `trial-cleanup`, `outbox-prune`, `processed-events-prune`, `parked-events-replay`, `trial-ledger-prune`); 6 metric-exporter goroutines inside `cmd/server` (not CronJobs); HPA 2–8; schema migrations run as Helm pre-install/pre-upgrade hook (`cmd/migrate`), not by replicas (`RUN_MIGRATIONS=false`); shutdown fits `terminationGracePeriodSeconds` 75 (HTTP + SQS drain ≤30s, outbox 30s, pools 10s); statement coverage **97.4%** (CI gate 95%, `TEST_INTERNAL_PKGS` in Makefile must list all packages carrying `_test.go` files). Fail-open/closed matrix: Catalog Service is the **one NOT fail-open** (`catalog_unavailable` 503 on cold-cache-plus-failure); Group Mapping, Delegation, and Token Service clients all fail open. Full detail in **[`.claude/operations.md`](operations.md)**.

## Claude Code Hooks & Agents

`.claude/settings.json` wires two hooks for Edit/Write/MultiEdit (they need `jq`; the guard fails closed without it):
- **PreToolUse `.claude/hooks/guard-invariants.sh`** blocks what CI would reject later: editing `.env`; editing a migration already on `origin/main` (MIG-1, add a new `NNNNNN_*.sql` instead); a non-`LOCAL` `SET app.tenant_id` (RLS-6); `database/sql`, `pgxpool.New*`/`pgx.Connect` in production Go; the SNS/SQS SDK outside `cmd/server/main.go`; `outbox.Enqueue*` outside `eventbus/publisher.go`; `internal/core/*` importing an adapter (service may import only `outbound/metrics`). Only the written text is checked, and Bash edits are not seen — CI stays the gate.
- **PostToolUse `.claude/hooks/gofmt.sh`** runs `gofmt -w` on each edited Go file.

**`invariant-reviewer` agent** (`.claude/agents/invariant-reviewer.md`, Read-only): reviews a diff against RLS, migration grants, outbox, idempotency/check-then-act, concurrency, cache, authorization, consumer, CD-19 access-matrix, layering and personal-data invariants. Use it before opening a PR: write `git diff main...HEAD > <scratch>/review.diff; git diff HEAD >> <scratch>/review.diff` and pass the absolute path.

## See Also

Detailed reference docs in `.claude/`:
- [`architecture.md`](architecture.md) — Clean Architecture directory tree, shared library integration, dep rules
- [`key-files.md`](key-files.md) — per-file descriptions with port contracts, adapter caveats, invariant references
- [`database-schema.md`](database-schema.md) — 10 tables, enums, RLS/tenant/seat/migration/trigger invariants
- [`api-caching-events.md`](api-caching-events.md) — full endpoint catalogue (P-*/I-*/O-*), authz invariants, cache keys, event catalogue, EVT-14/15/16
- [`request-flows.md`](request-flows.md) — provisioning, delegation, delegate-impact resolution (§8.8), invite→accept (§8.10), concurrency, GDPR
- [`operations.md`](operations.md) — security, observability, configuration, CI/CD, integration points, dependency degradation matrix

Full LLD (v2.44, "Org & Membership (Core) Service — Low-Level Design", reflects the post-decomposition end-state in this file), checked into this repo: [`docs/lld/iam-lld-org-membership-service.md`](../docs/lld/iam-lld-org-membership-service.md) — §16 open-question register, §17 error taxonomy, §19 migration strategy.

Observability docs in [`docs/observability/`](../docs/observability/README.md): how the standard is implemented here, the generated metric inventory (`metric-registry.md`), renamed/removed metric names (`migration.md`) and per-alert runbooks (`runbooks.md`).

Supplementary docs in the repo root:
- **`README.md`** — onboarding, prerequisites, quick-start, local dev setup
- **`ARCHITECTURE.md`** — detailed architecture narrative with diagrams
