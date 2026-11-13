# Changelog

All notable changes to this service are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning aligned with SemVer.

This service has never been deployed to any environment — there is no released version yet, so everything lives under `[Unreleased]`. Because of that, this file tracks only what is currently true and what is still outstanding, not a phase-by-phase build history; the full development diary (schema build-out, endpoint-by-endpoint rollout, since-removed subsystems) is available via `git log` if it's ever needed.

## [Unreleased]

### Removed (2026-11-13 — CD-54 seat-overage retirement, LLD rev 2.49)

- **CD-54: Seat-overage feature fully retired** — Prepaid seat model means overage cannot occur; member creation is blocked at SEAT-1 when `licensed_seats` are exhausted. `licensed_seats` reductions land only at renewal; Core never receives a value below seats in use (IAM-4). Removed:
  - `TenantSeatOverageStarted` and `TenantSeatOverageResolved` events, their Glue schemas (`tenant_seat_overage_started.json`, `tenant_seat_overage_resolved.json`), and their entries in `api/asyncapi.yaml` and `domain/event.go`. Membership event count on `iam.membership.events` reduced from 12 to 10.
  - `seat-overage-reconcile` CronJob (`cmd/reconciler/jobs/seat_overage.go` deleted, removed from `cmd/reconciler/main.go` registry, removed from `deploy/helm/values.yaml`). CronJob count reduced from 9 to 8.
  - `overage_since` column from `domain.Tenant`, `tenants` SQL queries (`tenantSelectColumns`, `scanTenant`), and all DTOs (`TenantResponse`, `SeatUsageResponse`). `SeatUsageResponse` / `SeatUsage` now returns only `{active_users, pending_invitations, licensed_seats}`.
  - `iam_org_membership_seat_overage_active` gauge, `iam_org_membership_seat_overage_started_total` counter, and `SeatOverageActive`/`SeatOverageStarted` package vars from `metrics/business.go`. Exporter goroutine count in `cmd/server/exporters.go` reduced from 5 to 4.
  - `SEAT_OVERAGE_GRACE_DAYS` env var, `SeatOverageGraceDays` from `jobs.Context`.
  - `membership-billing-q` SNS filter / queue reference from `api/asyncapi.yaml` (Billing no longer subscribes to membership events for overage enforcement).
  - `port.TenantRepository.LockSeatOccupancy` and `SetOverageSince` interface methods; `postgres.TenantRepository.LockSeatOccupancy`, `SetOverageSince`, and `ListSeatOverageCandidates`; `port.SeatOccupancy.OverageSince` field; `port.ReconcilerStore.ListSeatOverageCandidates`; `postgres.GaugeRepository.CountSeatOverageActive`.
  - `IAMSeatOverageActive` alert from `deploy/monitoring/app-alerts.yml`; `iam_org_membership_seat_overage_active` from the `iam:reconciler_markers:sum` SLO-3 formula in `deploy/monitoring/slo-rules.yml`; `IAMSeatOverageActiveApproachingGrace` alert from `deploy/helm/templates/prometheusrule.yaml`.
  - `iam_org_membership_seat_overage_active`/`seat_overage_started_total` rows from `docs/observability/metric-registry.md` (regenerated via `make metrics-inventory`).
  - SEAT-3, SEAT-4, SEAT-5 invariants marked retired in `database-schema.md`.

### Changed (2026-11-13 — dependency updates, reconciler fix, Go 1.26.11)

- **Go 1.26.11** — toolchain upgraded to fix `GO-2026-6859` (HTTP/1 server crash due to `MaxBytesReader` header race in `net/http`; `govulncheck` now reports `No vulnerabilities found`).
- **Module updates** — AWS SDK v2 (`aws-sdk-go-v2` v1.47.5→v1.48.0, config v1.33.13→v1.33.18, credentials v1.20.13→v1.21.0, glue v1.172.2→v1.175.1, sns v1.47.7→v1.48.0, sqs v1.52.8→v1.53.0 and transitive chain); `jackc/pgx/v5` v5.12.0→v5.13.0; `testcontainers-go` v0.44.0→v0.45.0 (and postgres module). `go mod tidy` applied; build and unit tests pass.
- **Reconciler context-cancellation fix** — `trial-cleanup`, `parked-events-replay`, `invitation-kc-cleanup`, and `realm-config-sync` job loops now check `ctx.Err()` before each iteration, matching the existing pattern in `outbox-prune`. When `RECONCILER_TIMEOUT` fires mid-loop, the job now exits cleanly rather than spinning through remaining items (each of which would fail with `context.DeadlineExceeded` and produce misleading failure-count log spam).

### Changed (2026-11-04 — TND-Q4 role gate widened, LLD rev 2.47)

- **I-13 assignee-override role gate widened: `tenant_admin` added (TND-Q4, confirmed 2026-11-04)** —
  Tender HLD v0.10 §10.3 priority note (2026-11-04) confirms the three-role list: `tender_admin`,
  `tenant_admin`, `tenant_owner`. `MembershipService.ValidateAndEmitAssigneeOverride` previously
  returned `403 insufficient_role` for `tenant_admin` actors; the role check in `membership_service.go`
  now includes `RoleTenantAdmin` alongside `RoleTenderAdmin` and `RoleTenantOwner`. Swagger doc
  comments in `internal_handler.go`, `docs/swagger/docs.go`, `swagger.yaml`, and `swagger.json`
  updated. Test comment in `test/unit/membership_removal_test.go` updated. Reference LLD
  (`Reference_doc/org_membership_lld_v12.md`) I-13 description updated.
  *(Previously: I-13 admitted only `tender_admin` and `tenant_owner`, matching Tender HLD v0.9 —
  that version is withdrawn by the Tender team's priority note of 2026-11-04.)*

### Added (2026-11-03 — I-20 internal departments endpoint, NEW-OM-WF-1 Workflow compat)

- **I-20 `GET /api/v1/internal/tenants/:id/departments` — service-to-service department list (TND-OM-P3):**
  Tender Service requires department names for display without a forwarded user session.
  `InternalHandler.ListDepartments` delegates to `DepartmentService.ListForTenant`, returns
  `{items: [{department_id, code, name, is_system, is_active, record_version}]}`. Route registered
  on the `tenantScoped` group, so `BindPathTenant` applies and the path tenant is the RLS tenant
  (IAPI-6). Gate: `RequireInternalCaller` (`iam-system` only); plain members receive `403`.
  New metric `iam_org_membership_department_list_total{result=success|error}` added for I-20
  observability; added to `docs/observability/metric-registry.md`. Endpoint count 41 → 42.
  `DepartmentService` wired into `NewInternalHandler` and `cmd/server/main.go`. All test harness
  call sites updated (`handler_ctors_test.go`, integration/e2e harnesses, path-tenant and
  eligibility postgres tests). Istio: `tenderCaller` namespace rule gains
  `notPaths: [/api/v1/tenants/*]` exclusion so the existing public tenant routes are unaffected;
  enabled in staging and prod overlays; CD-19 access-matrix test covers the new route.

- **NEW-OM-WF-1 — `x-internal-token` forwarded on Workflow Execution Service calls:**
  `workflow.HTTPClient` gains an `internalToken` field; `setInternalHeaders` now emits
  `x-internal-token` when `WORKFLOW_INTERNAL_TOKEN` is set in the environment (header omitted
  when blank — no behaviour change for local dev or any existing caller). Fixes the `401` every
  Workflow Execution API call returned because the Workflow Service's `RequireInternalToken`
  middleware was receiving no credential. `WORKFLOW_INTERNAL_TOKEN` documented in `.env-example`;
  token value pending cross-team credential agreement.
  Tests: `P10-WF-013` (token forwarded when set) and `P10-WF-014` (header absent when empty)
  added; all existing `NewHTTPClient` call sites updated for the new `internalToken string`
  parameter.

- **NEW-OM-WF-1 Helm — `WORKFLOW_INTERNAL_TOKEN` wired into `envFromSecret`:**
  `deploy/helm/values.yaml` adds `WORKFLOW_INTERNAL_TOKEN` to `envFromSecret` and a blank
  `secretValues` placeholder, following the same pattern as all other credentials. The
  Deployment template mounts it from the Kubernetes Secret as a `secretKeyRef`. Leave empty
  until the Workflow team provisions the credential per environment.

### Changed (2026-11-02 — asyncapi.yaml Workflow fan-out filter, NEW-OM-WF-1)

- **`membership-workflow-q` SNS filter extended: `MembershipRevoked` + `TenantMembershipsPurged`
  added (OM-WF-GAP-1, NEW-OM-WF-1):** `api/asyncapi.yaml` `receiveMembershipWorkflow` operation
  previously listed only `TenderAssigneeOverridden`, `DepartmentMembership*`, and
  `TenantStateChanged`. Two event types required by the Workflow Service were missing from both
  the filter string and the `messages` list:
  - `MembershipRevoked` — Workflow vacates per-assignment task ownership so a removed user does
    not hold active tasks (§8.8 removed-user cleanup).
  - `TenantMembershipsPurged` — Workflow terminates all instances for the tenant. This is the
    **only** signal emitted by the trial hard-delete path (`trial-cleanup` CronJob), which emits
    no `TenantStateChanged`.
  Both event types are now present in the filter string, `messages` list, and operation
  description. `receiveMembershipWorkflow` summary updated. The Workflow team must redeploy
  the `membership-workflow-q` SNS subscription from the updated spec to activate the filter
  change. Local floci `init-floci.sh` and the wire test (`TestWorkflowQueueCarriesBothEvents`)
  updated.

- **I-13 Swagger `@Description` updated for three-role actor list (Swagger contract fix):**
  `internal_handler.go` I-13 `@Description` now explicitly states that the actor must hold
  `tender_admin`, `tenant_admin`, or `tenant_owner` (Tender HLD §10.3, TND-Q4). Previously the
  annotation was silent on the accepted roles, leaving the Swagger contract out of sync with the
  implementation. `docs/swagger/docs.go`, `swagger.yaml`, and `swagger.json` regenerated.
  `membership_service.go` `ValidateAndEmitAssigneeOverride` doc comment updated to remove the
  ambiguous "tender_admin or higher" phrasing.

- **`.claude/api-caching-events.md` fan-out filter and I-14 contract corrections (docs only):**
  `membership-workflow-q` filter entry corrected (removed stale `TenderAssigneeOverridden` and
  `DepartmentMembership*` entries that were already in the description block but not reflected
  accurately in the filter list — OM-WF-GAP-1). I-13 actor roles table updated to list all three
  accepted roles explicitly. AuthZ Enrichment I-14 confirmed per-request behaviour and
  `x-auth-time` header contract (AE-46, AE-47) documented.

### Added (2026-11-04 — observability compliance, port/trace.go)

- **Observability compliance — OTel trace API confined to composition roots (`port/trace.go`):**
  All logs, metrics, and traces are now routed exclusively through `platform-gincommon`. A new
  `port/trace.go` exports five symbols that keep the OpenTelemetry trace API out of the port and
  service layers: `SpanContext` (value type carrying trace/span IDs), `ConsumerSpanFunc`
  (function type for constructing a consumer child span), `SetSpanContextExtractor` (injects the
  OTel-backed extractor from `cmd/`), `SetConsumerSpanFunc` (injects the OTel-backed span
  constructor from `cmd/`), and `WrapConsumerSpan` (called by consumers — delegates to the
  injected function). The `go.opentelemetry.io/otel/trace` import is now present only in
  `cmd/server/main.go` and `cmd/reconciler/main.go` (composition roots — permitted by
  `.go-arch-lint.yml`). No OTel API change is required by callers; the wire is transparent.

### Fixed (2026-11-04 — O-4 TenantSettingChanged, cache eviction, I-16 cap, metrics)

- **`OperatorService.SetFeatureFlags` (O-4) now emits `TenantSettingChanged`:** the event is
  enqueued atomically with the `feature_flags` write inside the existing transaction, carrying
  the authenticated operator's `actor_id` (from `requestctx`, not `SystemActorID`).
  Previously O-4 silently updated `feature_flags` without producing any event, so downstream
  consumers (AuthZ Enrichment cache invalidation, Audit Log) never learned of operator-driven
  flag changes. `api/asyncapi.yaml` updated to list O-4 as a source of `TenantSettingChanged`
  alongside P-2.

- **Cache eviction uses `context.WithoutCancel` to survive client disconnects (`invalidateMember`):**
  `invalidateMember` (and all callers — P-7, P-8, P-10, P-11, I-3, I-5, I-10, O-7) now derives
  the eviction context with `context.WithoutCancel(ctx)` before the Valkey `DEL`. Previously a
  client timeout or gateway disconnect cancelled `ctx` before the `DEL` completed, silently
  skipping cache eviction and leaving stale I-8 entries in Valkey for up to the 300 s ± 30 s TTL.
  CACHE-2/CACHE-9 invariants now hold under client disconnect.

- **`ListSubscriptionLapses` (I-16) SQL capped at `LIMIT 1000` rows per poll:** without a cap,
  a large billing backlog could cause I-16 to return an unbounded result set, exhausting memory
  in the Billing Service caller. The cap is applied in the repository query; Billing's existing
  pagination loop absorbs any continuation naturally. Added a unit test asserting the cap.

### Changed (2026-11-04 — metrics reclassification, dependency updates)

- **`iam_lifecycle_event_skipped_total` and `iam_lifecycle_event_lag_seconds` reclassified from
  Tier 2 to Tier 3:** a registry audit confirmed both metrics are emitted only by this service
  and not by any other IAM service. They are renamed:
  - `iam_lifecycle_event_skipped_total` → `iam_org_membership_lifecycle_event_skipped_total`
  - `iam_lifecycle_event_lag_seconds` → `iam_org_membership_lifecycle_event_lag_seconds`
  All emit sites, alert rules, dashboard panels, and `docs/observability/metric-registry.md`
  updated. The `Known gaps` note referencing both names as Tier-2 `proposed` entries is removed.
  `make metrics-lint` passes without `-strict` allowances for these names.

- **Dependency updates:**
  - `platform-gincommon/v2` v2.1.0 → v2.3.0 (additive; no API break — adopts new
    `WithoutCancel`-aware eviction helper used by the cache eviction fix above).
  - `github.com/jackc/pgx/v5` v5.11.0 → v5.12.0 (patch; connection reset reliability fix
    under PgBouncer transaction pooling).
  - AWS SDK patch updates: `aws-sdk-go-v2/service/sqs`, `aws-sdk-go-v2/service/sns`,
    `aws-sdk-go-v2/service/glue` bumped to their latest patch releases (no API surface change).
  `govulncheck ./internal/... ./pkg/...` → **0 vulnerabilities** after all bumps.

### Fixed (2026-11-01 — sysPool grants, LLD rev 2.46)

- **PUB-1 answered 500 when the migrator did not own the tables** (frontend integrated-deployment Issue 17). PUB-1, I-16, the reconcilers and the metric exporters run on the BYPASSRLS sysPool as `org_membership_migrator`. The migrations never granted that role anything, and BYPASSRLS skips policies, not privileges, so whenever another role ran the migrations every sysPool query hit `permission denied`. New migration `000007_migrator_grants` grants it `SELECT, INSERT, UPDATE, DELETE` on each table it does not own (`rls_violation_log`: `SELECT` only; nothing on `schema_migrations`), with default privileges for later tables. A grant it cannot make fails the migration rather than being skipped. New invariant MIG-10; postgres test against the real migrator role.
- **Local compose:** `org_membership_app` was the Postgres superuser, so RLS never applied locally. The superuser is now `postgres`, and `init-db.sql` creates the app role (`NOSUPERUSER NOBYPASSRLS`) and the migrator. Migrations and `SYSTEM_DATABASE_URL` run as the migrator, and `CATALOG_ADMIN_BASE_URL` is set; without it the server would not start. `.env-example` and the README match. Re-create the dev volume: `docker compose down -v`.

### Added (2026-10-30 — P-35 `slug`, LLD rev 2.45)

- **P-35 `GET /api/v1/me/session-context` returns `slug`** — the caller's own tenant workspace slug, from the `om:tenant` row it already reads. Frontend round-2 ask A: trial tokens carry no slug and PUB-1 returns none for an email, so without it the SDK could not cache a trial user's workspace or move the URL to `/t/<slug>/`. Additive field; Swagger regenerated; service, handler and e2e tests.

### Security (2026-10-30 — Trivy: tzdata DLA-4867-1 suppressed)

- **`.trivyignore` suppresses DLA-4867-1** (`tzdata` 2026b → 2026f, Trivy UNKNOWN), with `exp:2026-11-30`. The newest `gcr.io/distroless/static-debian12:nonroot` (`sha256:afa5c872…`) still ships 2026b, and the service never loads a time zone (all timestamps UTC). Same handling as DLA-4854-1. Remove both entries and re-pin once distroless ships tzdata ≥ 2026f.

### Added

- **Claude Code hooks and `invariant-reviewer` agent (2026-10-30):** `.claude/settings.json` wires a PreToolUse guard (`.claude/hooks/guard-invariants.sh`) that blocks edits CI would reject (`.env`, edits to a migration already on main, non-`LOCAL` `SET app.tenant_id`, `database/sql`/raw pgx pools, SNS/SQS SDK or `outbox.Enqueue` bypasses, core→adapter imports) and a PostToolUse `gofmt` hook. New Read-only `invariant-reviewer` agent checks a diff against the service's RLS, grant, outbox, idempotency, concurrency, cache, access-matrix and layering invariants. Mirrors `iam-realm-provisioner`'s setup, adapted to this repo's additive migrations. No code change.
- **`make trivy` + pre-push gate (2026-10-30):** `make trivy` builds the amd64 image (with the `go_private_token` build secret from `GO_PRIVATE_TOKEN` or `gh auth token`) and Trivy-scans it with CI's settings (CRITICAL/HIGH/UNKNOWN, fixed only, `.trivyignore`). New `.githooks/pre-push` runs it before every push (needs Docker; skip once with `SKIP_TRIVY=1 git push` or `--no-verify`); `make setup`/`make install-hooks` now install it too. Same as `iam-realm-provisioner`.

### Changed

- **CD-44: one AI-facing queue, `membership-rag-q` (LLD rev 2.42, 2026-10-28; TL decision):** LLM Retrieval & Generation's proposed queues on `iam.membership.events`, `iam-rag-purge-q` (`TenantMembershipsPurged`) and `iam-rag-membership-q` (`TenantStateChanged`), become **`membership-rag-q`**, with one filter carrying both event types. Docs only: LLD §7.3.2, the `api/asyncapi.yaml` topology comment, `.claude/api-caching-events.md`, README. Core only publishes to the topic; the LLM team owns and creates the queue and its subscription.

- **CD-50: events for a tenant with no row yet are parked and replayed, not retried until the DLQ (LLD rev 2.41, 2026-10-27; Commercial open item A, new EVT-18):** direct-paid signup calls RP-2 before I-1, and RP-2's `TenantRealmReady` / `DirectPaidSignup` / `TenantConverted(direct_signup=true)` usually reached `tenant-orgm-q` before the tenant row existed. EVT-17 retried them for only 4 deliveries (~2 minutes), then DLQ'd them, leaving the tenant `trial` in Core while Billing treated it as paid.
  - **Park:** with no tenant row, the consumer takes the tenant's provisioning advisory lock, re-checks the row, and stores the envelope in the new `pending_tenant_events` table (migration `000004_cd50_pending_tenant_events`, RLS + FORCE), acknowledging the message. If I-1 committed while it waited for the lock, it applies the event at once.
  - **Replay:** I-1 (`ProvisioningService.TrialSignup`, both the BFF call and the `TenantIdentityProvisioned` backstop) holds the same lock and, after commit, replays the tenant's parked events in `occurred_at` order through the normal projection (EVT-14 recency guard, EVT-16 relay, purge relay). A replay failure never fails I-1.
  - **Backstop:** new CronJob `parked-events-replay` (`*/5 * * * *`; 8 CronJobs) replays parked events for tenants whose row exists.
  - **Observability:** `iam_org_membership_tenant_events_parked_total`, `iam_org_membership_tenant_events_replayed_total{trigger}`, gauge `iam_org_membership_parked_tenant_events_stale` (sixth exporter goroutine), alert `IAMParkedTenantEventsStale` + runbook.
  - **Tests:** consumer unit tests (park, row-appears-under-lock, replay order, stays-parked rollback, failure stops the replay), I-1 lock/replay unit tests, the CronJob, and Postgres tests for the CD-50 case (RP-2's events before the BFF's I-1 → tenant ends `active`, `enterprise`, dedicated realm), the backstop path, the sweep, the stale gauge and RLS isolation.

- **CD-19: Istio on in staging + CI test of the access matrix (LLD rev 2.40, 2026-10-26; Commercial reply item 1):** `values-staging.yaml` now sets `commercialCallers.istio.enabled: true` (with the prod `existingCallerNamespaces`: `iam`, `gateway`, `observability`), so staging enforces the same L7 rules as prod instead of only the NetworkPolicy and the app-layer allow-list. New `test/helm/access_matrix_test.go` (build tag `helm`; `make helm-access-check`; CI step after `helm-check.sh` in `validate-quality.yml`) renders the chart with the staging and prod values and asserts, by parsing the YAML, the exact operations per principal (Billing, Entitlements, Signup BFF), the `workflow-app` rule, that the only unrestricted namespace rule is `[iam, gateway, observability]`, and that `billing`/`commercial`/`bff` are admitted in the NetworkPolicy only with their pod labels on the API port. The app-layer list (I-11, I-17, I-18) stays pinned by `TestCommercialSystemAllowedRoutes_StableKeys`. `helm` added to `ALL_TEST_TAGS` so vet/lint cover the new file.

- **`PlanCatalogChanged` respects T-9; one catalog queue (LLD rev 2.39, 2026-10-26; closes Commercial item 4):**
  - **Removed the `feature_flags` write.** The `catalog-orgm-plans-q` consumer (`catalog_plans_consumer.go`) merged each event's `feature_set` into every tenant's `feature_flags`, copying plan defaults into the per-tenant override delta (T-9) — a grant later removed from a plan would never have left any tenant. The consumer, `TenantRepository.MergeFeatureFlagsForPlan` (port, Postgres, no-op) and `SQS_CATALOG_ORGM_PLANS_QUEUE_URL` are deleted.
  - **Cache invalidation instead.** `catalog-orgm-q` is now the one queue on Catalog's `iam-catalog-events` (filter `DepartmentCatalogChanged`, `PlanCatalogChanged`). `PlanCatalogChanged` is `processed_events`-deduped (consumer `catalog`), clears `om:plans`/`om:plans:stale`, then sets `om:plans:gen:{code}` to the event id (unknown/missing code → every plan). I-8 reads the generation keys in the same `MGET` as `om:memberships:{tenant}:{user}` (no extra round trip), stores `plan_gen` with each entry, and treats a mismatch as a miss (CACHE-11): one `SET` retires the cached projections of every tenant on the plan, so the effective flags follow the catalog in seconds rather than after the 600 s + 330 s TTLs. A Valkey error in the consumer now returns an error (SQS redelivers) instead of being swallowed. `DepartmentCatalogChanged` handling is unchanged.
  - **Consumed schema fixed.** `schemas/plan_catalog_changed.json` required `plan_code`; Catalog sends `code` (with `change_type`, `feature_set`), so every real event would have failed validation and gone to the DLQ. Now Catalog's registered schema verbatim, in `api/asyncapi.yaml` too; pinned by `TestValidateConsumed_PlanCatalogChanged_MatchesCatalogShape`.
  - **Switched on.** `SQS_CATALOG_ORGM_QUEUE_URL` is a required startup variable in staging and production; local floci creates `iam-catalog-events` + `catalog-orgm-q` with the two-type filter and `.env-example` sets the URL; `TestCatalogQueueCarriesBothCatalogEvents` (wire test) pins the filter. Production queue + subscription: SRE/platform Terraform (see Known gaps).
  - **Tests:** consumer unit tests (order of clear-then-bump, dedup, every-plan fallback, retry on Valkey error, no `TenantRepository`), I-8 generation tests, and a Postgres test proving `feature_flags`/`record_version` stay untouched while I-8 drops a removed grant.

- **Dependabot: Moby test dependencies (2026-10-23).** `github.com/moby/go-archive` v0.2.0 → v0.3.3 (GHSA-hfg8-hc9c-6c3h, high: a crafted tar can write outside the extraction directory) and `github.com/moby/sys/user` v0.4.0 → v0.4.1 (GHSA-mjcv-p78q-w5fw, medium: unbounded parsing DoS). Both are indirect, pulled only by `testcontainers-go` in the test suites, never into the server binary; `testcontainers-go` itself is unchanged (v0.44.0).

- **Abandoned slug-conflict signups (LLD rev 2.38, 2026-10-22; Signup BFF open gap A-3):** the Signup BFF's mesh rule (`signupBffCaller`) now also admits exact `GET /api/v1/public/tenants/resolve` (PUB-1), for a server-side slug pre-check before RP-1 — a pre-check, not a reservation (races and offboarded slugs, T-6, still reach I-1's 409). New warning alert `IAMSignupProvisioningRejected` (both `prometheusrule.yaml` and `deploy/monitoring/app-alerts.yml`) on backstop-dead-lettered signups, with a runbook; such DLQ messages can be discarded — Realm Provisioner's new `orphaned-signup-reap` job removes the orphaned Keycloak user.

- **Signup BFF admitted per pod, I-1 only (LLD rev 2.37, 2026-10-22):** new Helm `signupBffCaller` (default enabled) implements Signup BFF decision BFF-1 / C-5, which never reached this chart (the BFF's I-1 call would have been denied). NetworkPolicy admits only `bff`-namespace pods matching `podSelector` (default `app.kubernetes.io/name: bff-signup`; an empty selector fails the render, so the whole `bff` namespace can never be admitted). The Istio AuthorizationPolicy admits only `cluster.local/ns/bff/sa/bff-signup`, on exact `POST /api/v1/internal/tenants`. LLD §10.2 / IAPI-1 updated.

- **platform-events/v2 v2.1.0 → v2.2.1 (2026-10-22):** handler-initiated dead-letters are settled once — after a successful `SendToDLQ` (handler ctx) the consumer deletes the message whatever the handler returns; a nil return after a *failed* `SendToDLQ` now leaves the message visible for retry (`platform_messages_failed_total{reason="dead_letter_error"}`) instead of deleting it; a message forwarded to the DLQ whose `DeleteMessage` fails is retried 3× and, if still undeleted, its redelivery to the same replica is deleted (12 h memory) instead of being forwarded again. v2.2.1 is the same code released with the go1.26.10 toolchain. Reviewed `sqspipeline.routeRejectsToDLQ` (EVT-15 / schema-violation rejects): it forwards with the handler's ctx and returns the original error when `SendToDLQ` fails — already the v2.2.0 contract, so no code change. `platform_dlq_messages_total` is still counted once by platform-events (this service never increments it).

- **Workflow services admitted to I-13 / I-15 / I-19 (LLD rev 2.36, 2026-10-22):** new Helm `workflowCallers` (default `workflow-app`). The NetworkPolicy only admitted same-namespace pods, so Workflow (in its own namespace) could not reach I-19/I-13/I-15; it now has a NetworkPolicy rule and, with Istio, an AuthorizationPolicy rule limited to those three routes. **Istio path fix:** the CD-19 rules used mid-path wildcards (`/api/v1/internal/tenants/*/…`), which Istio treats literally, so Billing and Entitlements were denied wherever the policy rendered (prod); they now use suffix matches (`notPaths` excludes the public `/seat-usage`). The policy fails to render with an empty `existingCallerNamespaces`.

- **deploy: platform namespace names gateway / observability (2026-10-22):** Helm NetworkPolicy ingress selector `envoy-gateway-system` → `gateway` (Envoy Gateway data plane) and metrics-ingress selector `monitoring` → `observability` (Prometheus), matching the platform namespace list (AWS dev environment HLD). Dev/staging/prod `commercialCallers.istio.existingCallerNamespaces` renamed the same way; `prometheus-adapter-rule.yaml` → `observability`.

- **Problem extension members via platform-gincommon (LLD rev 2.35, 2026-10-21):** `DomainError.Details` are attached with v2.1.0's `problem.Error.WithExtensions` and sent through `gincommon.AbortWithProblem`; the hand-written serialiser `abortWithDetailedProblem` is removed. Same top-level members on the wire (`record_version`, `active_workflows`, `licensed_seats`, `retry_after_seconds`, …), now with the library's logging, verified `tenantId` and span `traceId`; a detail can no longer overwrite a standard member such as `status` or `errorCode`.

- **platform-gincommon/v2 v2.0.0 → v2.1.0 (2026-10-21):** additive release — RFC 9457 extension members on problems (`Error.WithExtension`, `Builder.Extension`, `Problem.Extension`) and the library's go1.26.10 toolchain. No code change; problems without extensions serialise byte-identically. Adopting extension members in place of `abortWithDetailedProblem`'s hand serialisation is a separate follow-up. Stale library versions in VERSIONING and `.claude/` docs corrected (pgcommon v2.0.2).

- **Workflow queue filter (LLD rev 2.34, 2026-10-21):** `membership-workflow-q` now also accepts `MembershipRevoked` (a removed user's templates and assignments) and `TenantMembershipsPurged` (tenant purge — the only signal for a trial hard-delete, which emits no `TenantStateChanged`), per the Workflow Services' open question I2. Contract and local wiring only (`api/asyncapi.yaml`, `scripts/init-floci.sh`, wire test); no producer change. The production SNS filter is managed outside this repo.

- **Runtime CA bundle (DLA-4845-1, 2026-10-21):** distroless static-debian12 (pinned digest already the latest upstream) ships ca-certificates 20250419~deb12u1; deb12u2 removes the distrusted Comodo "AAA Certificate Services" and "emSign ECC Root CA - G3" roots. A new digest-pinned `debian:bookworm-slim` `certs` stage installs deb12u2 and its bundle replaces distroless's `/etc/ssl/certs/ca-certificates.crt` (verified in the built image: 150 → 148 certs, both roots absent). The Alpine builder's bundle is not used — it still carries the emSign root. `.trivyignore` gains `DLA-4845-1 exp:2026-11-21` (Trivy still reads distroless's dpkg metadata); `.docker-digests` and `make pin-base-images` (also brought up from a stale 1.26.6) cover the new stage. Remove all three once distroless ships deb12u2.

- **Go 1.26.10 (2026-10-21):** toolchain and the digest-pinned builder image (`golang:1.26.10-alpine@sha256:9ce231b2…f7ca`) bumped from 1.26.9, fixing GO-2026-6809 in `crypto/x509` (reachable via `auditlog.HTTPClient.WriteEntry` → `x509.Certificate.Verify`). `govulncheck` reports no reachable vulnerabilities.

- **EVT-17 — events for a tenant with no row yet are retried, not dropped (LLD rev 2.33):** delivery on `iam.tenant.events`/`billing.events` is unordered, so an event could arrive before its tenant was created and was recorded and discarded (e.g. a direct-paid `TenantRealmReady`, leaving the realm unset). Such events are now retried until the tenant exists or they reach the DLQ. Offboarded (soft-deleted) tenants and `TenantOffboarded` still drop.

- **API-2 guard on tenant-facing routes (LLD rev 2.32, 2026-10-21):** the 21 `/api/v1/tenants/:id/*` routes are now one `/tenants/:id` route group behind `RequirePathTenantMatch`, which rejects a path tenant that differs from `x-tenant-id` with `403 INSUFFICIENT_ROLE` before the active-tenant/active-member gates (no query on a mismatch). Same status and errorCode the handlers already returned; only the `detail` text of a mismatch changes. The header stays authoritative (no rebinding). Handlers keep their own check as defence in depth. New CI guard `TestPublicTenantRoutes_AllRequirePathTenantMatch`; `TestPublicTenantRoutes_PathsUnchanged` pins all 21 URLs.

- **Path tenant is authoritative on `/api/v1/internal/tenants/:id/*` (LLD rev 2.31, IAPI-6, 2026-10-21):**
  - **Why:** repositories filtered on the path tenant while RLS filtered on the tenant from `x-tenant-id`. When a caller's header and path disagreed, reads returned false `404`/`active:false`/`not_member` (and a cache hit vs. miss could disagree on I-9/I-11/I-14/I-17), and writes (I-3, I-10, I-13) failed RLS `WITH CHECK` and counted as RLS violations. It failed closed — no cross-tenant data was ever returned.
  - **What:** new `BindPathTenant` middleware on the `/internal/tenants/:id` route group binds the path tenant as `app.tenant_id` and `RequestContext.TenantID`; the caller's user, roles, client IP and user agent are unchanged. I-8 binds its `?tenant_id=` in the handler. The per-handler binding added for I-13/I-15/I-19 is removed. Public and operator routes are unchanged (the header stays authoritative there, API-2).
  - **Mismatch visibility:** WARN log `tenant_header_path_mismatch`, metric `iam_org_membership_tenant_header_path_mismatch_total{route,method,action}`, info alert `IAMOrgMembershipTenantHeaderPathMismatch` + runbook. The requested Tier-2 name `iam_tenant_header_path_mismatch_total` needs a platform-gincommon registry entry first, so the counter ships as Tier 3.
  - **Strict mode (off by default):** `STRICT_TENANT_PATH_VALIDATION` / Helm `iam.strictTenantPathValidation`. When on, a mismatch is rejected with `400 TENANT_MISMATCH` (RFC 9457, `{base}/iam/tenant-mismatch`) before the handler runs. Enable per environment after the mismatch counter has stayed at zero there.
  - **Compatibility:** no URL, request or response-shape change. Default behaviour differs only for a caller whose `x-tenant-id` disagrees with the path: it now gets the path tenant's correct answer instead of a false not-found.
  - **Tests:** middleware unit tests; `TestInternalTenantRoutes_AllBindPathTenant` route-registration guard (fails CI for a tenant route outside the group); Postgres regression through the real router for I-3, I-8, I-9, I-10, I-11, I-13, I-14, I-15, I-17, I-18, I-19.

- **RFC 9457 Problem Details adoption (LLD rev 2.29, 2026-10-18):** All HTTP error responses now use RFC 9457 `application/problem+json` via `platform-gincommon v2`'s shared implementation as the single error-handling owner.
  - `domainErrorToProblem` in `middleware.go` now maps every domain sentinel to the platform type (e.g. `TypeNotFound`, `TypeConflict`) and adds a domain-specific UPPER_SNAKE `errorCode` override via `.WithCode(strings.ToUpper(de.Code))` — e.g. `"TENANT_NOT_FOUND"` instead of the platform default `"NOT_FOUND"`, `"SEAT_LIMIT_REACHED"` instead of `"CONFLICT"`. All 50+ domain error codes now have stable, specific errorCodes.
  - `HandleError` routes domain errors that carry a `Details` map (e.g. `active_workflows`, `licensed_seats`, `record_version`, `retry_after_seconds`) through a new `abortWithDetailedProblem` helper that merges those fields as RFC 9457 §3.1 extension members into the `application/problem+json` body.
  - `AsyncAPIHandler` now uses `gincommon.AbortWithProblem(c, problem.NewInternalError(err))` — removes the last service-owned `gin.H{"code":…}` custom error JSON.
  - Swagger regenerated (`make swag`): `http.ErrorResponse` / `http.ValidationError` replaced by `http.ProblemResponse` (`= problem.Problem`); all `@Failure` annotations now reference the RFC 9457 envelope. `make swag-check` will pass after this commit.
  - 58 unit tests updated: `body["code"]` → `body["errorCode"]`, `body["message"]` → `body["detail"]`, domain code values updated to UPPER_SNAKE. `TestCD31_SeatLimitReached_CodeStringPinned` corrected — sentinel `.Error()` stays `"seat_limit_reached"`, HTTP wire `errorCode` is now `"SEAT_LIMIT_REACHED"`.
  - **CD-31 resolved**: this completes the deferred RFC 9457 migration noted in LLD rev 2.27.
  - `govulncheck ./internal/... ./pkg/...` → **0 vulnerabilities**. All 28 direct dependencies at latest versions (no updates available). All 21 packages pass `go test ./... -short`.

- **Dependency maintenance (LLD rev 2.28, 2026-10-15):**
  - `github.com/aws/aws-sdk-go-v2/service/glue` v1.157.0 → v1.171.0 (Glue Schema Registry SDK — minor; no API surface change for startup schema-resolution).
  - `github.com/aws/aws-sdk-go-v2/service/sqs` v1.52.1 → v1.52.4 (SQS consumer SDK — patch; long-polling reliability fix).
  - `github.com/jackc/puddle/v2` v2.2.2 → v2.2.3 (pgx connection-pool — patch; idle-connection eviction fix).
  - `github.com/alicebob/miniredis/v2` v2.39.0 → v2.40.0 (test-only Valkey stub — minor).
  - `govulncheck ./internal/... ./pkg/...` → **0 vulnerabilities** (1 vulnerability in a required module does not reach any call site in this service).
  - Swagger and AsyncAPI verified aligned against current code: all 39 endpoints, 14 produced events, and 15 consumed event types confirmed. No spec changes required.

### Fixed (2026-10-30 — CR-4 trial ledger hardening, LLD rev 2.44)

- **`trial_signup_ledger` grants.** Migration `000005` granted the app role nothing, so with a distinct `org_membership_app` role (staging/prod) every CR-4 check and every BFF I-1 carrying `email_hash` failed with `permission denied`. New migration `000006_cr4_trial_ledger_hardening` grants `SELECT, INSERT, UPDATE, DELETE` to the app role, revokes `PUBLIC`, and grants `SELECT` to `admin_readonly`.
- **One trial per email is enforced at I-1.** The ledger now stores the claiming `tenant_id`. I-1 claims the hash for its tenant and fails with `409 TRIAL_ALREADY_USED` (rolling the tenant back) when another tenant holds it. Before, `ON CONFLICT DO NOTHING` let two signups that both passed CR-4 each get a trial.
- **The hash is recorded on an I-1 replay.** The claim now runs before the replay short-circuit, so a tenant the backstop created without the hash still gets it when the BFF's I-1 retry arrives.
- **`email_hash` validated** on CR-4 and I-1: lower-cased, then 64 hex characters, else `400`. An oversized hash was a `500` inside I-1, and an upper-case spelling bypassed the ledger.
- **Retention.** New CronJob `trial-ledger-prune` (`30 4 * * *`) deletes ledger rows older than `TRIAL_LEDGER_RETENTION_DAYS` (default 730), so the email HMAC is not kept forever. The 730 days need the DPO's confirmation.
- **I-1 `licensed_seats`** was bound and silently dropped (hardcoded 10). Now `0` means the trial allowance of 10, a positive value is used (the BFF's direct-paid path), and a negative one is a `400`.
- **Istio:** the Signup BFF's rule now admits `POST /api/v1/internal/trial-eligibility`, which was denied (`403 RBAC`) to its only caller. The CD-19 access-matrix test pins it.
- CR-4 metric labels are now constants. Tests: handler (format, case), service (claim, replay, conflict, seats), job, and Postgres (migration grants, claim semantics, concurrent I-1 race, replay after backstop, prune).

### Added

- **P-35 `GET /api/v1/me/session-context` (LLD rev 2.43, 2026-10-29; frontend FE-Q4):** the signed-in user's own `tenant_id`, `roles`, `departments` [`{id, level}`], `plan`, `feature_flags`, `read_only`, `mfa_freshness_seconds`, `locale`, so the frontend SDK can render by role/plan/flag. No parameters — the caller is the gateway-injected `x-tenant-id`/`x-user-id`. New `SessionContextService` composes the I-8 projection (`om:memberships`; the exact source of AuthZ Enrichment's `x-tenant-roles`/`x-departments`/`x-plan`/`x-feature-flags`, with `read_only` appended to the flags as the header does) and the `om:tenant` row (I-14/I-9's source) — no new query. `RequireActiveTenant` applies; no membership row → `404 MEMBER_NOT_FOUND` (I-8's answer), suspended → `403`. No gateway or AuthZ Enrichment change needed. Swagger regenerated (new `me` tag). Tests: service (over a real `AuthZService`), handler/router.
- **PUB-1 at the edge (LLD rev 2.43, §10.2, §16 OQ-10 item 1):** `ingress.publicResolve` (HTTPRoute mode, off by default) renders `templates/publicresolve.yaml` — an Exact, GET-only HTTPRoute for `/api/v1/public/tenants/resolve`, a route-level SecurityPolicy without `mergeType` (replaces the gateway JWT + `ext_authz` on that route only) and a BackendTrafficPolicy with a per-client-IP global rate limit (`sourceCIDR` `Distinct`, 20/hour, PUBRES-3). Replaces the `httproute.yaml` TODO. CAPTCHA is a WAF item; the `tenant_id`-hint disclosure review stays open. `helm-check.sh` gains a fifth combo (HTTPRoute + publicResolve); `test/helm/public_resolve_test.go` pins the resources.
- **Main-route gateway rate limit on BackendTrafficPolicy (LLD rev 2.43, §10.2):** Envoy Gateway has no `rateLimit` on SecurityPolicy, so the old `ingress.securityPolicy.rateLimit` would have been rejected by the CRD. Moved to `ingress.rateLimit` (off by default) and rendered by the new `templates/backendtrafficpolicy.yaml` (route-level, `mergeType: StrategicMerge`); the main SecurityPolicy now renders only with CORS. `helm-check.sh`'s HTTPRoute combo enables it; `TestMainRoute_RateLimit_OnBackendTrafficPolicy` pins it.
- **P-6 sends `tenant_slug` to RP-5 (LLD rev 2.43):** `CreateInvitedUserRequest.TenantSlug` → RP-5's required `tenant_slug`, which RP uses for the invitation email's `https://app.bcbpsolutions.com/t/<slug>/` link. Sourced from the tenant row the P-6 seat pre-check already reads; always sent.

- **Gap 3 fix — I-2 response shape aligned with Swagger (2026-10-26):** `InternalTenantRealmResponse` now includes `RecordVersion int64 json:"record_version"` (was missing from the DTO despite the handler always sending it). The `PatchTenantRealm` handler now returns the typed struct instead of `gin.H{}`, so the Swagger `@Success 200 {object} InternalTenantRealmResponse` annotation matches the actual wire response. Callers (Realm Provisioner) that already read `record_version` are unaffected; callers relying on the Swagger spec now see the correct shape.

- **Gap 2 fix — DirectPaidSignup comment corrected (2026-10-26):** The `applyProjection` `DirectPaidSignup` case had an incorrect comment claiming RP's payload never sends a `plan` field. RP's `DirectPaidSignupPayload` does include `plan`; the fallback to `prevPlan` is intentional only for empty-plan payloads (older RP versions / safety net). Comment corrected; no behaviour change.

- **Gap 4 doc — Catalog Service not fail-open on I-1 (2026-10-26):** Explicit note added to README.md (I-1 row + Cross-service dependencies table) and ARCHITECTURE.md (ProvisioningService node) that I-1's two Catalog calls (`PlanByCode` + `Departments()`) run **before** the Postgres transaction and are NOT fail-open — `503 catalog_unavailable` on cold-cache + Catalog outage. bff-signup retries and the backstop handle this correctly; no code change needed.

- **Gap 1 deferred — `licensed_seats` Phase 1.5 TODO (2026-10-26):** Added a clearly-labelled Phase 1.5 comment in `ProvisionTenant` explaining that `req.LicensedSeats` is intentionally not forwarded to `TrialSignupInput` for trial signups (default 10 is correct). When Phase 1.5 (paid signup) activates: add `LicensedSeats` to `TrialSignupInput`, pass `req.LicensedSeats`, remove the hardcoded `10` from the tenant INSERT.

- **CR-4: Trial signup eligibility gate — `POST /api/v1/internal/trial-eligibility` (2026-10-26):** the Signup BFF's step-E pre-check before every RP-1/I-1 pair. Accepts `{"email_hash":"<HMAC-SHA256 hex of lower-cased email>"}` under the `RequireInternalCaller` (iam-system) gate; returns `200` when the email has not been used for a trial, `409 TRIAL_ALREADY_USED` when it has. Blocks Phase 1 trial signup end-to-end otherwise (BFF was getting 404).
  - **New table** `trial_signup_ledger(email_hash TEXT PK, created_at TIMESTAMPTZ)` — cross-tenant, no RLS, append-only; migration `000005_cr4_trial_signup_ledger`.
  - **Atomic ledger write in I-1:** `internalProvisionRequest` gains optional `email_hash` field; `TrialSignupInput.EmailHash` is passed through and written inside the TrialSignup transaction (ON CONFLICT DO NOTHING for idempotent replay). RP backstop and Realm Provisioner's own I-1 calls omit it — fail-open by design.
  - **New port/adapter:** `port.TrialSignupLedgerRepository` (ExistsEmailHash, InsertEmailHash); `postgres.TrialSignupLedgerRepository` implementation; `ProvisioningService.WithTrialLedger(...)` optional setter (same pattern as WithLogger — zero-impact on the 30+ existing test callers of NewProvisioningService).
  - **New error:** `domain.ErrTrialAlreadyUsed` ("trial_already_used") → 409 TRIAL_ALREADY_USED via domainErrorToProblem.
  - **New metric:** `iam_org_membership_trial_eligibility_check_total{result}`, pre-warmed for "eligible" and "already_used"; added to `docs/observability/metric-registry.md`.
  - **Route:** `internal.POST("/trial-eligibility", ...)` — on the `/internal` group, NOT under `/tenants/:id` (no path tenant pre-signup). Not subject to `BindPathTenant` or the `TestInternalTenantRoutes_AllBindPathTenant` guard. Endpoint count 40 → 41.
  - **Tests:** 6 unit tests in `trial_eligibility_handler_test.go` (eligible, already_used, missing hash, DB outage, nil-ledger fail-open, auth gate); all existing unit tests unaffected.

- **Signup provisioning backstop (LLD rev 2.33, Signup BFF BFF-2, 2026-10-21):** the `tenant-orgm-q` consumer handles RP's new `TenantIdentityProvisioned` (RP-D16, emitted by RP-1/RP-2 once the admin's Keycloak user exists). If the tenant has no row, it runs I-1 from the payload, so a signup whose BFF-side I-1 call never succeeded still completes; an existing (or offboarded) tenant is left alone. A payload I-1 can never accept goes to the DLQ (`provisioning_rejected`); transient failures retry. New metric `iam_org_membership_tenant_provisioning_backstop_total{result}`. Consumed schema `tenant_identity_provisioned.json` + AsyncAPI message; `tenant-orgm-q`'s declared SNS filter gains the type (**update the production subscription filter to match**).

- **I-19 assignee eligibility read for the Workflow Services (LLD rev 2.30, 2026-10-21):** `GET /api/v1/internal/tenants/:id/members/:user_id/eligibility?department_id=&required_level=` → `{tenant_id, user_id, department_id, required_level, eligible, reason, role_level}`. Answers the Workflow Services' 2026-10-14 request (Definition publish check; Execution instance start with overrides, admin override, reassignment, delegation reroute). Eligible iff the membership is active, the department is active for the tenant, and the held level is at least `required_level` (preparator < reviewer < approver, owned by this service — same rule as I-13). `reason` ∈ `not_member`/`member_not_active`/`department_not_active`/`not_in_department`/`level_insufficient`. Never 404; uncached; `iam-system` only. New metric `iam_org_membership_assignee_eligibility_check_total{result}`. Endpoint count 39 → 40.

- **Production-readiness hardening (LLD rev 2.20):**
  - **Go 1.26.9:** toolchain bumped from 1.26.6, resolving three stdlib `net/http` CVEs — GO-2026-6617 (HTTP/2 HPACK encoder race → server crash), GO-2026-6519 (request smuggling), GO-2026-6612 (double flow-control refund). `govulncheck` reports 0 vulnerabilities.
  - **Library bumps:** `platform-gincommon` v1.5.0→v1.6.0, `platform-events/v2` v2.0.0→v2.1.0, `platform-pgcommon/v2` v2.0.0→v2.0.1, `golang.org/x/net` v0.59.0→v0.61.0 (upstream CVE), AWS SDK patch, OTel v1.46.0→v1.47.0, `prometheus/client_golang` v1.24.1→v1.25.0.
  - **P-4 N+1 eliminated:** `MembershipService.List` previously issued 1+N×2 queries per page (101 at limit=50). Now issues 3 total via new `ListByUsers(ctx, tenantID, []uuid.UUID)` batch methods (`WHERE user_id = ANY($2)`) on `TenantRoleRepository` and `DeptMembershipRepository`.
  - **`iam_org_membership_audit_delivery_failed_total{entry_type}`:** new Tier-3 counter incremented when fire-and-forget AL-5 goroutines return an error (previously silently discarded). Entry types: `config.tenant_setting.changed`, `invitation.created`, `invitation.revoked`, `invitation.expired`. New `IAMAuditDeliveryFailed` alert (severity: ticket, 10-min window) + runbook in `docs/observability/runbooks.md`.
  - **`AUDIT_LOG_BASE_URL` / `AUDIT_LOG_TIMEOUT_MS`:** added to `deploy/helm/values.yaml` and `validateRequiredEnv` (`devOK: true`, `stagingWarn: true`). AL-5 calls remain fail-open; without the URL all audit writes are skipped silently.
  - **2 new CI enforcement scripts:** `check-database-access.sh` (no `database/sql`, no bare pgxpool construction, no `pgcommon.Config{DSN}` outside `postgres/db.go`, all `RunInTx*` wrapped by `wrapConnErr`) and `check-events-platform.sh` (outbox writes isolated to `eventbus/publisher.go`, `inbox.NewStore`/`store.Process` isolated to `postgres/idempotency_repository.go`, `events.InitMetrics` in both cmd binaries). Both wired into `validate-quality.yml`.

### Fixed

- **Helm: the route SecurityPolicy now merges with the Gateway-level policy** (`ingress.securityPolicy.mergeType`, default `StrategicMerge`, rendered into the SecurityPolicy spec only when non-empty). Without `mergeType`, Envoy Gateway applies only the most specific SecurityPolicy, so enabling this chart's HTTPRoute policy REPLACED the Gateway's on these routes — dropping its JWT validation and ext_authz (AuthZ Enrichment, which sets the trusted identity headers; IAM HLD §11.1). `mergeType` requires Envoy Gateway ≥ v1.8; set `""` only on an older gateway, and there leave the policy disabled. Dormant by default (`securityPolicy.enabled: false`).
- **I-8 cache eviction (CACHE-7/8, 2026-10-21):** only P-28, I-4 and O-7 evicted `om:memberships:{tenant}:{user}`. Dept assign/level change/remove (P-10/P-11), suspend/reactivate (P-7), removal (P-8), I-5 delete, I-10 SAML JIT and I-3 accept/rejoin left I-8 serving the previous projection for up to the 300 s ± 30 s TTL, so a demoted, removed or suspended user kept their old standing for AuthZ Enrichment. All now evict after commit. Covered by `test/unit/memberships_cache_eviction_test.go`.
- **`make run` panic on a fresh floci container:** `scripts/init-floci.sh` never registered the `TenantSettingChanged` schema (a produced event on `iam-tenant-events` since rev 2.27), so `GlueCodec`'s startup lookup failed. Now registered; the script's produced-schema count is 15.

- **P-2 `PATCH /tenants/:id` validation gaps:** `tenant_service.go` now enforces (a) `len(*patch.Name) ≤ 255` (same cap as provisioning; DB had only `CHECK(name <> '')`) and (b) BCP-47 structural regex on `default_locale` (same `^[a-zA-Z]{1,8}(-[a-zA-Z0-9]{1,8})*$` as provisioning; previously only empty string was rejected). Both return `400 invalid_request`.
- **Port-layer OTel import violation:** `internal/core/port/logger.go` imported `go.opentelemetry.io/otel/trace` directly (forbidden vendor import in the port layer). Trace-ID extraction is now injected via `port.SetTraceExtractor(fn func(context.Context) string)`, called once per binary after `gincommon.InitTracingWithConfig`. The OTel import moves to `cmd/server/main.go` and `cmd/reconciler/main.go` (composition roots — permitted).
- **arch-lint violation:** `internal/adapter/outbound/auditlog` was imported by `cmd` but not declared as a component in `.go-arch-lint.yml`. Added to `adapters_outbound`.

### Removed

- **`TrialTenantProvisioned` consumption (2026-10-21):** Realm Provisioner retired the event (nothing is deployed) and emits `TenantIdentityProvisioned`, a strict superset, from RP-1 and RP-2 instead. O&M's no-op handler, consumed schema `trial_tenant_provisioned.json`, AsyncAPI message and `tenant-orgm-q` filter entry are removed; an in-flight one is acknowledged as an unknown type. RP has deleted its Glue schema, so it is also gone from `schema-prune.yml`'s shared-registry guard, which now lists `TenantIdentityProvisioned` instead.

- **Mock server files:** `tools/mockwf/main.go` (Workflow Service mock) and `scripts/mockserver/main.go` (multi-service mock) deleted. Both are superseded by Go test fakes (`scenarioCatalogClient`, etc.) and `testcontainers-go` integration tests. The `make mock-servers` Makefile target is removed.

### Added (continued)

- **PUB-1: `GET /api/v1/public/tenants/resolve` (identifier-first login, IAM HLD §8.2.7)**: a new, deliberately unauthenticated route, the only one in this service. It closes the pre-login realm-discovery gap that neither the HLD nor this LLD's login flows previously specified.
  - **What it does:** resolves a company slug, or the email of a still-pending, unexpired invitation, to `{found, realm, strategy, tenant_id?}`. `tenant_id` appears only for a shared/trial tenant (PUBRES-4). It is always `200`, never `404`: a no-match is exactly `{"found":false}` (constant-shape anti-enumeration, PUBRES-2).
  - **Auth and pool:** it bypasses `ext_authz`/AuthZ Enrichment and this service's own auth/GUC middleware entirely. `registerPublicRoutes` registers it on the engine, outside the protected `/api/v1` group. Queries run against the BYPASSRLS `sysPool`, the same pool as I-16, since no `app.tenant_id` GUC is ever bound on this path. It is deliberately uncached, with no event and no write.
  - **Scope decision (LLD §16 OQ-11, option A):** an already-active member's email is **not** resolvable today, since O&M stores no email for active members. Option B, an iam-user-profile email lookup, is deferred.
  - **Code:** `PublicHandler`, `TenantResolutionService`, and `TenantRepository.ResolveByIdentifier` (one `UNION ALL` statement; a slug beats an email; the most recent invite wins).
  - **Schema, metric, error:** migration `000002_pub1_pending_invitations_email_idx` (additive `idx_pi_email_pending`); metric `iam_org_membership_public_tenant_resolve_total{result}`, pre-warmed; error code `invalid_identifier` (reusing `ErrValidation`).
  - **Tests:** every tier, including `TestContract_PUB1_AntiEnumeration_ShapeAndTiming` (PUBRES-2's timing/shape contract) and a sweep asserting every other `/api/v1` route still 401s with no headers.
  - **Not done here:** the Envoy `ext_authz` bypass route and gateway rate-limit/CAPTCHA (LLD §16 OQ-10) remain platform config, marked with TODOs in the Helm templates. AsyncAPI is intentionally unchanged, since PUB-1 emits no event. See LLD §5.4/§8.11 (rev 2.13/2.14).

- **`ConsumerHeartbeat` readiness probe (OBS-1)**: `cmd/server/consumer_heartbeat.go` — a thin `atomic.Bool`-backed probe that each SQS consumer goroutine holds; `MarkDead()` is deferred so `/readyz` returns 503 (and removes the pod from rotation) the moment any consumer goroutine exits. Wired into all three consumer goroutines via `RouterConfig.Consumers` in `cmd/server/wiring.go`.
- **`sqsDLQAdapter` / `dlq_adapter.go` (DLQ routing hardening)**: the only place in this binary that calls SQS transport methods (`SendMessage`, `GetQueueAttributes`) directly — exempted in `.github/scripts/check-forbidden-events-bypass.sh` Rule-2 so the bypass-guard still catches accidental direct-SQS use elsewhere. Separates raw AWS SDK usage cleanly from the DLQ pipeline logic in `dlq.go`.
- **OBS-4: I-8 cache-hit SLI recording rules + `IAMSLO1b_CacheHitRateLow` alert** (`deploy/monitoring/slo-rules.yml`): `iam:i8_latency_hit_sli:ratio_rate_5m`/`_1h` recording rules; slow-burn alert fires when the 1h cache-hit rate (requests served ≤15ms) drops below 90% for 5 minutes — the primary Valkey degradation signal for I-8.
- **OBS-8: bounded-cardinality metric pre-initialization** (`internal/adapter/outbound/metrics/business.go`): `registerMetrics` now pre-warms all known label combinations for `platform_messages_{received,processed,failed}_total` (both SQS queues) and `platform_dlq_messages_total` (all consumed event types × both DLQ reject reasons) so counters appear at 0 on startup rather than only after the first event arrives. Also adds `ObserveLifecycleEventLag(eventType, seconds)` — the SLO-3 primary drift signal recorded on every consumed lifecycle event.
- **Migration `000001_check_constraints`**: two additive DB-level check constraints — `chk_full_name_length` (SECURITY-2: `pending_invitations.full_name ≤ 512 chars`, backing the existing service-layer validation to prevent direct-SQL bypass) and `chk_display_name_length` (SECURITY-9: `dept_role_labels.display_name ≤ 255 chars`, sole enforcement point until a service-layer check is added). Forward-only per MIG-1.
- **Contract tests for I-8 and I-15 response shapes** (`test/postgres/contract_test.go`, TEST-7): pin every top-level field of both internal API responses against a real Postgres container — a field rename, drop, or type change fails CI before reaching any downstream consumer (AuthZ Enrichment, Tender ACL Service, Delegation Service).
- **Runbooks**: `docs/runbook-i8-latency.md`, `docs/runbook-outbox-dead-letter.md`, `docs/runbook-rls-cross-tenant.md` — on-call response steps for the three highest-impact alert classes.
- **Monitoring dashboards**: `deploy/monitoring/dashboards/business-health.json`, `data-integrity.json`, `i8-latency.json` — Grafana dashboard definitions for the three observability tiers.

- **AUTH-9 defense-in-depth reject (IB-3)**: `InvitationService.Invite`/`AddFromRegister`, `DeptMembershipService.Assign`, and `MembershipService.ReconcileRoles` now reject a target subject that resolves to a `service_account`-typed Keycloak principal with `403 service_account_not_grantable`, ahead of the pre-existing structural guarantee (the composite FK from `tenant_roles`/`dept_memberships` to `tenant_memberships`, TR-8/DM-4, which already made a non-member ungrantable). Backed by a new outbound client, `internal/adapter/outbound/tokenservice` (`port.TokenServiceClient.IsServiceAccount`, `TOKEN_SERVICE_BASE_URL`/`TOKEN_SERVICE_TIMEOUT_MS`, default 1000ms), which calls Token Service's new `GET /service-accounts?principal_sub=` (TS-5) lookup and **fails open**: an empty/unreachable Token Service degrades to allowing the operation rather than blocking it, since the FK bar remains the primary guarantee. See LLD §2.4/AUTH-9.
- **`internal/adapter/inbound/consumer/catalog_consumer.go`**: the SQS consumer for `catalog-orgm-q` (Gap 12) — `DepartmentCatalogChanged` clears `om:departments`; `PlanCatalogChanged` clears `om:plans` and bumps `om:plans:gen:{plan}` (LLD rev 2.39, see Changed).

### Fixed

- **Observability environment made explicit:** every knob platform-gincommon reads from the environment is now set in Helm and `.env-example` instead of left to implicit defaults: `OTEL_TRACES_SAMPLER_RATIO` (0.1 in Helm, 1.0 in dev), `OTEL_PROPAGATOR_BAGGAGE`, `OTEL_TRACES_IGNORE_REMOTE_PARENT_SAMPLED`, `LOG_LEVEL` and `LOG_SAMPLING`. The migrate Job receives the log settings too. `APP_NAME` is the one source of the service name; the unread `OTEL_SERVICE_NAME` was removed. `.env-example` no longer claims that an empty OTLP endpoint or `OTEL_*_EXPORTER=none` disables export — gincommon falls back to `localhost:4317` and does not read those variables.
- **Second library-incorporation audit, LLD rev 2.17:**
  - **No HTTP calls inside a transaction:** the dept-membership demotion gate (WFI-9/12) called Delegation and Workflow inside the retried write transaction, holding row locks and a pooled connection for two HTTP timeouts and repeating both calls on a deadlock retry. It now runs before the transaction.
  - **Event ordering made real:** `outbox.EnqueueOrdered` requires that transactions for one key never interleave, but several paths locked nothing that identified the key. `enqueue` now takes a transaction-scoped advisory lock on the key first. The ordering claim in the docs was also corrected: SNS/SQS delivery is not ordered end to end.
  - **Reconciler timeouts:** the CronJobs inherited the API's 5s `PG_STATEMENT_TIMEOUT`, which would fail trial-cleanup's cascading delete once the backlog grew. They use `RECONCILER_STATEMENT_TIMEOUT` (120s). `PG_LOCK_TIMEOUT` (2s) now bounds row-lock waits.
  - **Migrations out of the replicas:** a `cmd/migrate` Helm pre-install/pre-upgrade hook Job migrates once per release, and the Deployment runs with `RUN_MIGRATIONS=false`. A dirty migration or an image older than the database fails the Job with a repair hint.
  - **Metrics on gincommon's constructors:** all 19 Tier-3 collectors use `gincommon.NewCounterVec`/`NewGauge`/`NewHistogramVec`, which validate names and labels at startup. The five counters that were never incremented are wired: seat limit, invite throttling, removals/demotions blocked by delegates, removal resolutions, and seat overage. The ownerless and seat-overage counters now count after commit. `unknown_event_acknowledged_total` drops its constant `topic` label. Dependency endpoint labels are snake_case.
  - **platform-events:** the library's `events.GlueDecodeCodec` replaces the local decoder, which skipped the compression and JSON checks. Queue-depth sampling is on (`SQS_QUEUE_DEPTH_INTERVAL` 60s). `processed_events.event_id` is `uuid`.
  - **New alerts with runbooks:** outbox ordering blocked, outbox backlog, DLQ backlog, DB pool saturated, DB acquire errors, DB retries exhausted.
  - **HTTP:** gincommon's own 400 gets the `invalid_request_context` code. The real writer is restored if a handler panics. A raw statement/lock timeout or exhausted deadlock maps to its proper code instead of 500. `db_unavailable` keeps the original Postgres error in its cause chain. Both pools drain in parallel at shutdown.
  - **Tests and CI:** the inbound pipeline moved to `internal/adapter/inbound/sqspipeline` and is tested against floci (poison pill forwarded on first delivery; transient failure forwarded at receive 4). `check-metric-naming.sh` requires gincommon constructors. The bypass and outbox-access checks cover SDK subpackages, batch methods and `outbox_dead_letters`.
- **platform-gincommon v1.4.0 → v1.5.0:** the four Tier-2 `iam_*` metrics are now registry entries, so they are built with `gincommon.NewShared*Vec` (name, help, labels and buckets from the registry) instead of hand-written collectors, and `metricslint.sh` no longer carries an allowance for them. `iam_rls_violations_total{violation_type}` values outside the registry's fixed set are recorded as `other` (`metrics.AddRLSViolations`).
- **Library-incorporation audit (platform-events v2, platform-pgcommon v2, platform-gincommon v1.4.0), LLD rev 2.16:**
  - **Tenant events could be lost (high):** `RoutingPublisher.PublishBatch` returned on the membership lane's error before publishing the tenant lane, and the outbox runner then marked the unattempted `TenantCreated`/`TrialStarted` records published. Both lanes are now always attempted and their failures merged into one `*events.BatchError`.
  - **Exactly-once consumption:** `processed_events` dedup now runs through platform-events' `pkg/inbox` (`port.Inbox` → `inbox.Store.Process`). The claim and the projection commit in one transaction, so two concurrent deliveries of one event can no longer both pass a pre-check. CI rejects hand-written SQL against the table.
  - **Prune jobs capped at one batch a day:** `outbox-prune` now loops `PrunePublished` until a batch comes back short; `processed-events-prune` uses `inbox.Store.Prune`, which loops itself.
  - **Seat-overage events could be dropped:** the reconciler ignored `Enqueue` errors, so `overage_since` could change without its event. The error now rolls the change back.
  - **Statement timeout:** `PG_STATEMENT_TIMEOUT` is applied by pgcommon per transaction (`Config.StatementTimeout`) instead of as a DSN option, which corrupted a `DATABASE_URL` with no query string, could be rejected by PgBouncer, and also cut migrations short.
  - **pgcommon error helpers:** SQLSTATE string matching replaced by `IsOperatorIntervention`/`IsSystemError`/`IsPoolClosed`; `statement_timeout` (57014) and `lock_timeout` (55P03) are now 503 `request_timeout` instead of `db_unavailable`. The system pool has its own `pool="system"` metrics label.
  - **Metric label vocabulary:** `iam_org_membership_dependency_call_failures_total{outcome}` became `{reason}` with `timeout`/`transport`/`http_4xx`/`http_5xx`/`http_other`/`fallback_served` (it used the registry's fixed `outcome` label outside `success|failure|error`, and counted 4xx and transport errors as `5xx`). CI now runs platform-gincommon's `metricslint` (`make metrics-lint`).
  - **HTTP:** `/metrics` uses `gincommon.MetricsHandler` (a failing collector no longer blanks the scrape). On a request deadline, `HandleError` now leaves the 503 to gincommon so `platform_http_request_timeouts_total` counts it. A client disconnect is recorded as 499 instead of a 5xx. `NormalizeAuthErrors` buffers only 401s and adds `ambiguous_identity_headers`. The docs 401 uses the standard error envelope. Outbound calls forward `x-request-id`.
  - **Shutdown fits the 75s grace period:** consumers stop in parallel with the HTTP drain, and the pools drain on their own budget. New `SQS_DRAIN_TIMEOUT` (15s) and `SQS_HANDLER_TIMEOUT` (20s) defaults.
  - **Reconciler:** the same service identity and envelope `Source` as the server; metrics pushed to `PROMETHEUS_PUSHGATEWAY_URL`; CronJob envelope sentinels now actually set.
  - **Outbox:** the v1-era TEXT payload override and hex-decode trigger on `outbox_events` are removed (v2 fixed the bug they worked around). Events with a subject use `outbox.EnqueueOrdered`, so one user's events publish in order.
  - **Tracing:** `gincommon.NewSpanTracer` replaces the hand-written pgx tracer. The consumer span is now an internal child of the library's `sqs.receive` span, and it links to the originating request only when both IDs are present.
- **Library alignment (platform-events v2, platform-pgcommon v2, platform-gincommon v1.4.0), LLD rev 2.15:**
  - **DLQ:** the hand-rolled raw-SQS DLQ adapter (`cmd/server/dlq_adapter.go`) is gone; consumers use platform-events' own `DLQPublisher` and `WithDLQForwarding`. Malformed bodies are now forwarded to the DLQ instead of silently dropped. Permanent rejects (EVT-15, EVT-17) forward the original message, not a re-encoded copy, and stay on the queue if the send fails. `SQS_MAX_RECEIVE_COUNT` defaults to 4 so the library forwards before SQS redrive (5) takes over. A queue with no valid RedrivePolicy now fails startup; a transient lookup failure only warns. The IAM policy gained `sqs:GetQueueUrl` and the `catalog-orgm-q` ARNs.
  - **DLQ metric double-count:** `platform_dlq_messages_total` is now counted only by the library. The reject reason moved to new Tier-3 `iam_org_membership_consumed_event_rejected_total{event_type,reason}`, and the EVT-15 / schema-violation alerts were migrated to it. New `IAMMalformedInboundMessage` warning.
  - **Request timeout reported as 409:** a deadline expiring during a pgcommon retry wait now maps to 503 `request_timeout`. Deadlock or serialization retries that are exhausted map to 409 `conflict` with `details.code = "concurrent_update"`.
  - **Request timeout outside observability:** the timeout now runs through `gincommon.Config.RequestTimeout`, innermost, so logs and metrics record the 503 the client actually got.
  - **`NormalizeAuthErrors` marked every response as written:** it wrote an empty body even when the handler wrote nothing, which suppressed gincommon's timeout 503. It now forwards only what the handler wrote.
  - **Environment identity:** one resolved `APP_ENV` (fallback `ENVIRONMENT`, default `dev`) is now passed explicitly to metrics, tracing (`InitTracingWithConfig`) and the logger, in both binaries. Previously they could disagree.
  - **`iam_auth_session_revoke_failed_total{reason}` used the raw HTTP status code** (e.g. `"503"`), which the central registry rejects. Non-2xx responses now map to `http_4xx`/`http_5xx`/`http_other`, alongside `transport`. The metric name is unchanged.
  - **Config:** `MIGRATION_DATABASE_URL` is required whenever `PG_BOUNCER_MODE=true`. The BYPASSRLS `sysPool` defaults to 0 min conns (bouncer mode) unless `PG_MIN_CONNS` is set.

- **Trivy: suppress `DLA-4792-1` in `.trivyignore`**: `gcr.io/distroless/static-debian12:nonroot` carries `DLA-4792-1` (UNKNOWN severity, fixable) in `tzdata 2026b-0+deb12u1` — a timezone database update, not a security vulnerability. The CI Trivy gate fails on UNKNOWN+fixable (`ignore-unfixed: true`), causing exit-code 1. The fixed package (`tzdata 2026c-0+deb12u1`) is not yet available in any published distroless image, so a SHA bump cannot resolve this. Added `.trivyignore` suppressing `DLA-4792-1` with an inline comment explaining the rationale and a removal reminder for when distroless ships tzdata 2026c. Go binaries remain clean (0 findings).

- **OBS-5: SLO-3 composite gauge recording rule corrected** (`deploy/monitoring/slo-rules.yml`): `iam:reconciler_markers:sum` previously used a weighted expression that over-counted some markers by 5×; replaced with a plain sum of all four gauge metrics (`iam_org_membership_tenant_ownerless + iam_org_membership_realm_sync_pending + iam_org_membership_seat_overage_active + iam_org_membership_pending_invitations_stale`). `seat_overage_active` was also missing from the prior rule.

- **`deploy/helm/values.yaml` production-readiness pass**: added the missing `TOKEN_SERVICE_BASE_URL`/`TOKEN_SERVICE_TIMEOUT_MS` entries for the new AUTH-9 outbound client (added above but never wired into the chart — same class of gap as the `DELEGATION_BASE_URL` one below). Also caught `DELEGATION_TIMEOUT_MS` still hardcoded to the pre-Gap-8 default of `300` in this chart — deploying it as-is would have silently overridden the Gap-8 code-default fix (300ms → 1000ms) back to the broken value on every real deploy. Corrected to `1000`.
- **`TestP19RPEdges_PatchRealmConfig_BadBaseURL_RequestBuildError`** (`internal/adapter/outbound/realmprovisioner/http_client_edges_test.go`) called `PatchRealmConfig` with an empty `port.RealmConfigPatch{}`, which the `PatchRealmConfig` request-body fix (below) now short-circuits to `nil` before ever building a request — so the test no longer exercised the bad-base-URL path it was meant to. Fixed by giving it a non-empty patch, matching every sibling test in that file.
- **`RealmProvisionerClient.PatchRealmConfig` request body**: sent `{"local_accounts_enabled": bool}`, but Realm Provisioner's handler expects the generic `{"setting_key": "local_accounts_enabled", "desired_value": bool}` envelope — every T-15 local-accounts-toggle patch was silently ignored by RP. Fixed to send the envelope RP actually reads.
- **`delegationcheck.HTTPClient` default timeout** (Gap 8): raised from 300ms to 1000ms — 300ms was too tight under load or cold-start, causing the §8.8.4 dept-scope precision lookup to fall back to tenant-wide impact scoping more often than intended. `DELEGATION_TIMEOUT_MS` still overrides it. Also added a startup `Warn` log when `DELEGATION_BASE_URL` is unset, so a missing config is visible immediately instead of silently degrading on first call.
- **`catalogadmin.HTTPClient.setInternalHeaders`'s `x-tenant-id: uuid.Nil` workaround** (Gap 11) was previously undocumented; added a comment explaining why it's sent (CAT-I1/CAT-I2 are cross-tenant calls with no real tenant to scope to, and the identity-bridge middleware accepts uuid.Nil as a well-formed placeholder it never reads) and the risk if that middleware behavior ever changes.
- **`InvitationService.Invite`'s seat-cap-lost-race path** (`internal/core/service/invitation_service.go`) intermittently reported the wrong outcome to one racer under concurrent invites: `TxRunner.RunInTx` retries its callback on a deadlock/serialization failure, but the closure set one of two mutually-exclusive outer-scope result variables (`created`/`seatLimitErr`) without resetting either between invocations, so a stale value from an earlier, rolled-back attempt could leak into the attempt that actually committed. Fixed by resetting both at the top of the closure on every invocation.
- **CI `test/postgres` reliability**: bounded `pgcommon.NewPool` connections with a per-attempt timeout + retry (a stalled connection attempt previously had no deadline anywhere above it, so it could leak a goroutine indefinitely and starve a *later*, unrelated test's own connection attempt into the same hang); lowered `TEST_POSTGRES_PARALLEL` for CI to reduce concurrent-container resource pressure; raised the CI job and internal test timeouts to match.
- **`TestConsumerEVT14_StaleEventSkipped`** (`test/postgres/consumer_evt_test.go`) intermittently failed on Linux CI runners only: it wrote a full-nanosecond-precision `time.Time` into a `timestamptz` column, which `pgx` truncates to microsecond precision on encode, then compared the re-read (truncated) value against the original (untruncated) one — a mismatch that depends on the OS clock's actual sub-microsecond jitter. Fixed by rounding to microsecond precision before the comparison, matching the sibling test that already did this. The production EVT-14 guard itself was confirmed correct — this was a test-only bug.

### Known gaps

- **Tier-2 `iam_*` metrics awaiting ratification:** platform-gincommon v1.5.0 registers `iam_rls_violations_total` (emitted by this service, iam-audit-log and iam-user-profile), `iam_auth_session_revoke_failed_total`, `iam_lifecycle_event_skipped_total` and `iam_lifecycle_event_lag_seconds` (emitted only by this service; kept Tier 2 because the concepts are IAM-wide) as `proposed` entries with no owner (registry OQ-4). `metricslint check` passes; they stay errors only under `-strict` until IAM names an owner and governance ratifies them.
- **`iam_lifecycle_event_lag_seconds` overlaps `platform_event_propagation_seconds`:** the platform metric measures envelope time to first receipt; this one measures envelope time to apply, including retries. Governance decides whether to keep both.

- `domain.ErrGroupMappingUnavailable` (`group_mapping_unavailable`, 503) is declared per LLD §17's error taxonomy, but `GroupMappingService.resolveMappings` deliberately fails open and never actually returns it (ADR-0007 Action Item 4). This is a known mismatch between the declared taxonomy and current behavior, not a bug — flipping Group Mapping to fail-closed is a deliberate future decision, not in scope now.
- **Gap 12 infrastructure:** both catalog events are published by `iam-catalog-admin` and consumed on `catalog-orgm-q`, but the production queue and its SNS subscription are SRE/platform Terraform that does not exist yet, and Catalog's `deploy/messaging/sns_infrastructure.tf.example` still filters `catalog-orgm-q` to `DepartmentCatalogChanged` only — it must add `PlanCatalogChanged` (LLD rev 2.39, §16 OQ-12).

### Current architecture notes

- `/metrics` is served from its own `http.Server` on `METRICS_PORT` (default `9090`), separate from the API's `APP_PORT` (`8080`) — mirrors `iam-tender-acl`'s split. This lets the monitoring-namespace `NetworkPolicy` grant reach only the metrics port instead of the whole API surface (NetworkPolicy filters by port, not path).
- The four-service decomposition (ADR-0007 + ADR-0008) is complete on this branch: departments/plans catalog ownership, SAML group→dept/role mapping, tender ACL overlays, and delegation grants/OOO coordination have all moved to their respective standalone services (Catalog/Admin Config, Group Mapping/JIT Config, Tender ACL, Delegation). This repo — "Core" — retains only the organizational layer. See `.claude/CLAUDE.md` for the current scope and ownership boundaries.
