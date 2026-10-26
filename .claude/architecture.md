# Architecture

Refines HLD §5.6, §15.3. Clean Architecture / Ports-and-Adapters — dependencies point inward; `core/domain` imports nothing external; `cmd/server/main.go` (HTTP server), `cmd/reconciler/main.go` (CronJob dispatcher, `--job=<name>`) and `cmd/migrate/main.go` (schema migration hook Job) are the three composition roots.

## Package Layout

```
iam-org-membership/
├── cmd/
│   ├── server/
│   │   ├── main.go                     # HTTP composition root, pool + middleware wiring, 3 SQS consumers, outbox runner, graceful shutdown; runMigrationsAtStartup() (RUN_MIGRATIONS)
│   │   ├── wiring.go                   # buildTopicPublisher / buildTopicCodec (one SNS publisher + Glue codec per topic)
│   │   ├── exporters.go                # runBusinessExporters — the 5 metric-exporter goroutines over pgadapter.GaugeRepository
│   │   ├── consumer_heartbeat.go       # per-queue ConsumerHeartbeat health check
│   │   ├── identity.go                 # APP_ENV/ENVIRONMENT resolution
│   │   └── swagger_info.go
│   ├── migrate/
│   │   └── main.go                     # pgadapter.MigrateSchema (outbox.ApplySchema, then domain migrations); MIGRATE_TIMEOUT (10m); explainMigrationError hints; run once per release by the Helm pre-install/pre-upgrade hook Job
│   └── reconciler/
│       ├── main.go                     # CronJob dispatcher, --job=<name> (or RECONCILER_JOB); RECONCILER_TIMEOUT (5m), RECONCILER_STATEMENT_TIMEOUT (2m) on both pools; Pushgateway push
│       ├── identity.go
│       └── jobs/                       # invitation-expiry, invitation-kc-cleanup, realm-config-sync, seat-overage-reconcile, trial-cleanup, outbox-prune, processed-events-prune (7 jobs) + context.go (jobs.Context)
├── internal/
│   ├── core/
│   │   ├── domain/                     # entities, value objects, DomainError; NO external deps
│   │   │   ├── tenant.go               # Tenant, TenantPlan, SubscriptionStatus
│   │   │   ├── department.go           # Department, TenantDepartment
│   │   │   ├── membership.go           # TenantMembership, DeptMembershipView, MembershipListPage
│   │   │   ├── role.go                 # TenantRole, TenantRoleCode, DeptMembership, DeptRoleLabel
│   │   │   ├── group_mapping.go        # GroupDeptRoleMapping, GroupTenantRoleMapping, GroupDeptMapping (resolution shapes; Group Mapping Service owns the tables)
│   │   │   ├── plan.go                 # Plan, PlanPatch, BrandingLevel — client-side cache representation; Catalog Service owns the table
│   │   │   ├── invitation.go           # PendingInvitation, InvitationDeptMapping, SeatUsage
│   │   │   ├── event.go                # DomainEvent envelope, TopicForEvent
│   │   │   ├── event_payloads.go       # per-event payload structs
│   │   │   └── errors.go               # ErrNotFound, ErrConflict, ErrWorkflowResolutionRequired, ErrSeatLimitReached, ErrInvitationAlreadyExists, ErrInvalidReplacement, ErrDBUnavailable
│   │   │                               # NO delegation.go, NO acl.go — moved to Delegation Service / Tender ACL Service
│   │   ├── port/                       # interfaces required by the core
│   │   │   ├── tenant_repository.go    # incl. ListSubscriptionLapses (I-16) and ResolveByIdentifier (PUB-1) — both sysPool-only (BYPASSRLS)
│   │   │   ├── authz_repository.go     # AuthZRepository — the I-8 four-table join, moved out of authz_service.go so core/service depends on a port, not *pgcommon.Pool directly
│   │   │   ├── department_repository.go # TenantDepartmentRepository — per-tenant activation junction only; the global catalog itself is external
│   │   │   ├── membership_repository.go # MembershipRepository, TenantRoleRepository, DeptMembershipRepository, DeptRoleLabelRepository
│   │   │   ├── invitation_repository.go
│   │   │   ├── reconciler_store.go     # ReconcilerStore — sysPool-bound cross-tenant reconciler queries
│   │   │   ├── idempotency.go          # Inbox (ProcessOnce) + InboxPruner (Prune) — consumer dedup over platform-events pkg/inbox
│   │   │   ├── catalog_reader.go       # (ADR-0007) DepartmentCatalogReader, PlanCatalogReader — narrow read interfaces backing CatalogService
│   │   │   ├── group_mapping_client.go # (ADR-0007) GroupMappingClient — replaces the old local group_mapping_repository.go
│   │   │   ├── outbound_clients.go     # WorkflowClient (§8.8), RealmProvisionerClient, DelegationCheckClient (ADR-0008), TokenServiceClient (AUTH-9), CatalogAdminClient (ADR-0007) — NO UserProfileClient
│   │   │   ├── cache.go                # Valkey interface (advisory)
│   │   │   ├── event_publisher.go
│   │   │   ├── logger.go
│   │   │   └── tx_runner.go            # TxRunner + WithTx/TxFromContext (context-carried tx)
│   │   │                               # NO delegation_repository.go, NO acl_repository.go, NO user_profile_client.go, NO standalone workflow_client.go/realm_provisioner_client.go (folded into outbound_clients.go)
│   │   └── service/                    # use cases
│   │       ├── tenant_service.go
│   │       ├── department_service.go
│   │       ├── membership_service.go   # RemoveUser — delegate-impact pre-check + resolution (§8.8); AcceptInvitation (I-3 branch); tenant_roles cascade (TR-9)
│   │       ├── dept_membership_service.go # dept-level assign/remove gating (demotion gate runs before RunInTx); calls DelegationCheckClient for dept-scoped precision; on outage Remove degrades to tenant-wide, a demotion proceeds ungated
│   │       ├── invitation_service.go   # Invite (§8.10), RequiredActions, PI-11/PI-12 throttles, SEAT-1
│   │       ├── role_label_service.go
│   │       ├── provisioning_service.go # I-1 TrialSignup, slug/locale validation, I-5 cascade (ownerless counter after commit)
│   │       ├── operator_service.go
│   │       ├── authz_service.go        # I-8 hot path; depends on port.AuthZRepository, not *pgcommon.Pool — the four-table join itself lives in the postgres adapter, this owns TR-7/PLAN-6 composition
│   │       ├── catalog_service.go      # (ADR-0007) Read-through + stale-if-error cache over CatalogAdminClient — NOT a table owner
│   │       ├── group_mapping_service.go # client-side resolve over GroupMappingClient — NOT a table owner, fails open
│   │       ├── subscription_lapse_service.go # (I-16, §16 OQ-9/RP-C3) Cross-tenant read; its TenantRepository MUST be sysPool-bound (BYPASSRLS), not the RLS-scoped app pool
│   │       ├── tenant_resolution_service.go # (PUB-1, LLD §5.4/§8.11) Pre-login slug/pending-invite-email → realm lookup; sysPool-bound like I-16 (no GUC on this route); no cache, no event
│   │       └── cache_keys.go           # om:* key builders
│   │                                   # NO delegation_service.go, NO acl_service.go, NO role_service.go (renamed role_label_service.go — tenant_roles grants now live in membership_service.go)
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                   # Gin handlers, DTOs
│       │   │   ├── router.go           # single source of truth for the route table, shared by cmd/server/main.go and the e2e test harness
│       │   │   ├── tenant_handler.go
│       │   │   ├── department_handler.go
│       │   │   ├── membership_handler.go # DELETE pre-check; POST .../removal-resolution (P-26); seat-usage (P-27); reset-mfa (P-34)
│       │   │   ├── dept_membership_handler.go
│       │   │   ├── role_label_handler.go
│       │   │   ├── invitation_handler.go # invite lifecycle (P-6/P-30/P-31)
│       │   │   ├── operator_handler.go
│       │   │   ├── internal_handler.go # I-1..I-16, incl. I-15 GET .../members/:user_id/exists (grant-time check for Delegation/Tender-ACL services) and I-16 GET /subscription-lapses (RP-C3 bulk read)
│       │   │   ├── public_handler.go   # PUB-1 GET /api/v1/public/tenants/resolve — the one unauthenticated route; registered by registerPublicRoutes on the engine, outside the protected group (PUBRES-1)
│       │   │   ├── middleware.go       # GUCBridgeMiddleware, NormalizeAuthErrors, RequireActiveTenant/Membership, RequireOperatorRole/SystemRole, HandleError (DomainError/pg error → status + code)
│       │   │   ├── errors.go           # ErrorResponse construction
│       │   │   ├── docs.go / asyncapi.go / swagger_*.go # gated Swagger UI + AsyncAPI viewer (DOCS_ENABLED)
│       │   │   └── dto.go
│       │   │                           # NO delegation_handler.go, NO acl_handler.go, NO group_mapping_handler.go (admin group-mapping CRUD moved to Group Mapping Service)
│       │   ├── consumer/
│       │   │   ├── membership_event_consumer.go # SQS handler: iam.tenant.events, billing.events (EVT-14 recency, EVT-15 clamp, EVT-16 relay)
│       │   │   ├── dedup.go            # processOnce/ackUnknown over port.Inbox (platform-events inbox.Store) — shared IDEMP-1..4 helpers, used by both consumers
│       │   │   └── catalog_consumer.go # SQS handler: catalog-orgm-q ← DepartmentCatalogChanged (clears om:departments) / PlanCatalogChanged (clears om:plans, bumps om:plans:gen:{plan} — CACHE-11, LLD rev 2.39; never writes feature_flags); unknown types through dedup.go's ackUnknown
│       │   └── sqspipeline/            # the inbound pipeline every queue runs (moved out of cmd/server; the old cmd/server/dlq.go, dlq_adapter.go, inbound_schema.go are gone) — importable so test/integration runs it against floci
│       │       ├── consumer.go         # EnvForQueue (defaults: SQS_MAX_RECEIVE_COUNT 4, SQS_DRAIN_TIMEOUT 15s, SQS_HANDLER_TIMEOUT 20s, SQS_QUEUE_DEPTH_INTERVAL 60s), NewConsumer (events.GlueDecodeCodec + events.WithDLQForwarding), consumer span
│       │       ├── dlq.go              # RequireDLQ (startup preflight), routeRejectsToDLQ (permanent-reject shortcut)
│       │       └── inbound_schema.go   # Validator/ValidatorFunc, validateConsumed, Handler (DLQ router → consumer span → validateConsumed → Handle)
│       └── outbound/
│           ├── postgres/               # repository impls + db.go (pools, wrapConnErr), migrate.go (MigrateSchema/RunMigrations), idempotency_repository.go (InboxRepository), gauge_repository.go, reconciler_store.go, authz_repository.go; tenant_repository.go's ResolveByIdentifier is PUB-1's single UNION ALL lookup
│           │   └── migrations/         # 000000_initial_schema (consolidated — never deployed, so the decomposition history was squashed), 000001_check_constraints, 000002_pub1_pending_invitations_email_idx (idx_pi_email_pending for PUB-1)
│           ├── valkey/                 # cache impl (go-redis/v9)
│           ├── eventbus/               # Publisher (outbox enqueue, ordered per <tenant_id>/<subject>), RoutingPublisher (2 topics); GlueCodec (publish, version by definition); ValidatingCodec over embedded schemas/*.json; consumers use the library's events.GlueDecodeCodec (no local decoder)
│           │   └── schemas/            # schema-governance workspace, //go:embed-ed
│           ├── metrics/                # business.go (Register, every collector) + registry.go (PlatformRegistry ratification status) — the "observability" component in .go-arch-lint.yml
│           ├── httpx/                  # httpx.NewClient(timeout) — shared otelhttp-instrumented http.Client, forwards x-request-id
│           ├── workflow/               # HTTP client for Workflow Service — retained, unrelated to the delegation extraction
│           ├── realmprovisioner/       # HTTP client for Realm Provisioner
│           ├── catalogadmin/           # (ADR-0007) HTTP client for Catalog / Admin Config Service (departments, plans — read-only)
│           ├── groupmappingclient/     # (ADR-0007) HTTP client for Group Mapping / JIT Config Service
│           ├── delegationcheck/        # (ADR-0008) HTTP client for Delegation Service's dept-delegate lookup
│           └── tokenservice/           # (AUTH-9/IB-3) HTTP client for Token Service's TS-5 service-account lookup
│                                       # NO userprofile/ package — dead code once delegation's OOO coordination moved out, deleted (LLD §16 OQ-5)
├── pkg/requestctx/                     # context.go (RequestContext, WithRequestID/RequestIDFromContext), guc.go (WithSystemTenant/WithTenant)
├── api/
│   ├── asyncapi.yaml                   # AsyncAPI 3.0 — iam.membership.events + iam.tenant.events channels (design-time source of truth)
│   └── embed.go                        # embeds asyncapi.yaml (the "apispec" arch-lint component)
│                                       # openapi.yaml DELETED — REST contract is now docs/swagger/* (generated via `make swag` from handler annotations), not hand-authored
├── docs/
│   ├── swagger/                        # generated by `make swag` — swagger.yaml/swagger.json/docs.go
│   ├── lld/                            # iam-lld-org-membership-service.md (LLD v2.20)
│   ├── observability/                  # README.md, metric-registry.md (generated, `make metrics-inventory`), migration.md, runbooks.md
│   ├── architecture/                   # README + mermaid diagrams
│   └── runbook-*.md                    # i8-latency, outbox-dead-letter, rls-cross-tenant, schema-registry
│                                       # docs/schema-changelog.md and docs/schema-archive/ are written by schema-registry.yml / schema-prune.yml on main — not present yet
├── deploy/
│   ├── helm/                           # Helm chart: deployment, migrate-job (pre-install/pre-upgrade hook), cronjobs (7), hpa, pdb, networkpolicy, prometheusrule, secret, servicemonitor, …
│   ├── monitoring/                     # app-alerts.yml, slo-rules.yml, schema-registry-alerts.yml, prometheus-adapter-rule.yaml, dashboards/ (business-health, data-integrity, i8-latency)
│   └── iam/                            # IAM policy (policy.json, policy.tf.example)
├── test/{unit,postgres,integration,e2e,dbseed}/   # dbseed/pool.go = shared pgcommon pool for tests; integration/pipeline_dlq_test.go runs sqspipeline against floci
├── Dockerfile  docker-compose.yml  Makefile  go.mod  .golangci.yml  .go-arch-lint.yml
```

`internal/adapter/outbound/eventbus/schemas/` **is the one schema-governance workspace** — there is no separate `internal/eventschema/` directory. `schema-gov extract --schema-dir internal/adapter/outbound/eventbus/schemas` generates the Draft-07 files directly from `asyncapi.yaml` into this location, which doubles as the runtime copy embedded via `//go:embed schemas/*.json` in the `ValidatingCodec`; `.github/workflows/schema-registry.yml` validates/diffs/registers against this same path; `schema-gov register` uploads to AWS Glue Schema Registry. There is no separate design-time-vs-runtime split.

## Shared Library Dependencies (HLD §15.4)

```go
require (
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2     v2.1.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2        v2.2.1
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2      v2.0.2
)
```

**Not a Go module:** `platform-schemagov` is a Python 3.12 CLI (`schema-gov`) at v0.4.0, installed from source in CI (`pip install git+…/platform-schemagov.git@v0.4.0`, authorized by the org `GO_PRIVATE_TOKEN` secret) because the GHCR package's Actions-access allow-list doesn't include this repo and `docker pull` returns `denied`; the `ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4` image is used only by local `make` targets — zero Go-code presence.

**Not a dependency:** `iam-keycloakclient` — this service **never calls the Keycloak Admin API**; that is Realm Provisioner's exclusive responsibility (HLD §4.2).

### `platform-gincommon` integration

`router.go` (`NewRouter`) chains, in order: a 1 MB body cap → `gincommon.ObservabilityMiddlewares` (`PanicRecovery → RequestID → Tracing → CorrelationHeaders → Metrics → Logging → Timeout`; the timeout comes from `Config.RequestTimeout`, 30s default, and runs innermost so a deadline 503 is the status that gets logged and counted) → `NormalizeAuthErrors()` (G-13: buffers only a 400 or 401 and adds a `code` to gincommon's bare body — `invalid_request_context` on ContextMiddleware's 400, `missing_identity_headers` or `ambiguous_identity_headers` on RequireAuth's 401; a body that already has a `code` and every other status pass straight through). On a request-deadline error `HandleError` aborts without writing, so the library's timeout middleware writes its 503 and counts `platform_http_request_timeouts_total`; a client disconnect is recorded as 499 — then, per `/api/v1` sub-group, `gincommon.ProtectedMiddlewares` (`RequireAuth → ContextMiddleware`) → this repo's own `GUCBridgeMiddleware` → `RequireJSONContentType`, followed by route-specific gates (`RequireActiveTenant`/`RequireActiveMembership` on public `/tenants/*`, `RequireOperatorRole` on `/operator/*`, `RequireSystemRole` on `/internal/*`). `ServiceName` comes from `APP_NAME` (default `iam-org-membership`) — the single source of the service name for logs, traces, metrics and the SQS consumer name; `OTEL_SERVICE_NAME` is not read. `/metrics` is served on `METRICS_PORT` by `gincommon.MetricsHandler()`. **No gRPC server** — Gin HTTP only. Each of the six outbound clients (Workflow, Realm Provisioner, Catalog Admin, Group Mapping, Delegation Check, Token Service — no User Profile client any more, that adapter was deleted as dead code) builds its `http.Client` via `httpx.NewClient(timeout)`, whose `otelhttp`-wrapped transport injects W3C `traceparent` and emits a client span automatically — not a hand-rolled `propagateTraceparent` helper, and not `gincommon.PropagateHeaders` either (that's the inbound-focused helper; this is the outbound leg). (A prior revision of this doc described a manual per-package `propagateTraceparent` helper as the current mechanism and `httpx` as unadopted scaffolding — verified stale via direct grep, corrected here.)

### `platform-pgcommon` integration

- `NewPool` + `ConfigFromEnv`, `PGBouncerMode` env-driven.
- **`GUCProvider = GUCSetFromContext`** — binds `app.tenant_id` **transaction-locally** via `set_config(..., is_local => true)` on **every** checkout, reads included. Never a session-scoped `SET` (would persist on a pooled backend and leak across tenants; RLS-6). This is the security-critical invariant that makes RLS safe under PgBouncer transaction pooling.
- Writes run in `pgcommon.RunInTxWithRetryOpts` (`TxRunner`, retries 40P01/40001), reads in `pgcommon.RunInTx` (`withPool`); consumer dedup runs in `inbox.Store.Process`'s own transaction — no bare non-transactional query path exists. Outbound HTTP calls are never made inside a transaction (the WFI-9/12 demotion gate runs before it).
- Error mapping: `IsUniqueViolation` / `IsForeignKeyViolation` / `IsCheckViolation` / `IsDeadlock` / `IsSerializationFailure`.
- `Pool.Health`, `migrate.Runner` (domain migrations and `outbox.ApplySchema`, both with the logger; the library's default 30s migration `lock_timeout`), `pgmetrics.InitWithIdentity`, `gincommon.NewSpanTracer` as `Config.Tracer` (pgcommon wraps it in its query tracer).
- `Config.StatementTimeout` from `PG_STATEMENT_TIMEOUT`, applied per transaction by pgcommon (never a DSN startup option, which PgBouncer may reject); migrations get none. The BYPASSRLS system pool has `PoolName = "system"`, so its `platform_db_*` series stay separate from the app pool's `default`.
- Error classification via `IsConnectionException`/`IsInsufficientResources`/`IsOperatorIntervention`/`IsSystemError`/`IsPoolClosed` (503 `db_unavailable`), `IsQueryCanceled`/`IsLockNotAvailable` (503 `request_timeout`) and `IsDeadlock`/`IsSerializationFailure` (409 `concurrent_update`) — no SQLSTATE string matching.
- **`Config.SlowQueryThreshold`** from `PG_SLOW_QUERY_THRESHOLD` (200 ms in `.env-example`/Helm), logged at WARN through the same Zap sink via `pgadapter.NewLoggerAdapter`.

### `platform-events` integration — RoutingPublisher

Unlike `iam-user-profile` (single-topic producer), O&M wires its own local **`eventbus.RoutingPublisher`** (HLD §9.2) — not a `platform-events` type; that library only exposes `NewSNSPublisher` for a single topic. It holds two pre-built `events.Publisher` fields (`membership`, `tenant`), not a topic-ARN map, and routes each envelope via **`domain.TopicForEvent(env.Type)`** — keyed on event **type**, not `Envelope.Source`: `TenantCreated`/`TrialStarted` → `tenant`, everything else → `membership`. This is the one meaningful wiring difference from User Profile.

**Substantive consumer of two topics**, plus a cache-invalidation-only third queue, `catalog-orgm-q` ← Catalog's `DepartmentCatalogChanged`/`PlanCatalogChanged` (`SQS_CATALOG_ORGM_QUEUE_URL`, required in staging/prod since LLD rev 2.39):
- **`iam.tenant.events`** via `tenant-orgm-q` — Realm Provisioner tenant-lifecycle events (`TenantIdentityProvisioned` — signup provisioning backstop, `TenantRealmReady`, `TenantConverted`, `DirectPaidSignup`, `TrialExpired`, `TrialReactivated`, `TenantSuspended`, `TenantOffboarded`, `TenantReactivated{source=operator}` — new, resolves F1 of the RP↔O&M alignment review, RP-10 reversing RP-14). O&M **never self-consumes** its own `TenantCreated`/`TrialStarted` (HLD §9.1.1 "No self-consumption" — produce/consume sets are disjoint).
- **`billing.events`** via `billing-orgm-q` — Billing Service events (`TenantPlanChanged`, `TenantPaymentPastDue`, `TenantSubscriptionCancelled`, `TenantReactivated`, `TenantSeatsChanged`).

Queue naming: `<topic-short>-<consumer-short>-q` (HLD §9.1). Consumer short-name `orgm`. Each queue has a `-dlq` (`tenant-orgm-q-dlq` / `billing-orgm-q-dlq` / `catalog-orgm-q-dlq`) with RedrivePolicy `maxReceiveCount = 5`; platform-events forwards at `SQS_MAX_RECEIVE_COUNT` 4, one below it. Idempotency via `processed_events` (platform-events inbox).

## Database Access Rule (enforced in CI — two complementary layers)

**All database connections, configuration, and operations pass through platform-pgcommon exclusively.** The five-rule enforcement matrix:

| What | Required | Forbidden |
|---|---|---|
| Pool creation | `pgcommon.NewPool` | `pgxpool.New/Connect`, `pgx.Connect`, `sql.Open` |
| Configuration | `pgcommon.ConfigFromEnv()` + `pgadapter.DSNFromEnv/SystemPoolConfig/MigrationDSNFromEnv` | bare `pgcommon.Config{DSN:…}` struct literal outside `postgres/db.go` |
| Transactions | `pgcommon.RunInTx` / `RunInTxWithRetryOpts` wrapped by `wrapConnErr` | naked `tx.Exec/Query` without going through TxRunner |
| Error classification | `pgcommon.Is*` functions (IsDeadlock, IsSerializationFailure, IsQueryCanceled, IsPoolClosed …) | raw SQLSTATE string comparison |
| Migration | `pgmigrate.Runner` (via `postgres.MigrateSchema`) | direct `pgx` DDL or `database/sql` migration drivers |

**Layer 1 — `golangci-lint` forbidigo** (`.golangci.yml`, `analyze-types: true`): bans `pgxpool.New/NewWithConfig/Connect` and `pgx.Connect(Config)` and `sql.Open` at the resolved identifier level — not the call-site text, so a trailing `\(` in the pattern would silently match nothing; verified against a deliberate sanity-check violation before trusting the rule. `forbidigo` bans the construction *functions*, not the *types* — two files legitimately import `pgxpool` as a type (`test/dbseed/pool.go`, `test/e2e/security_test.go`) and pass cleanly.

**Layer 2 — `check-database-access.sh`** (`.github/scripts/check-database-access.sh`, belt-and-suspenders Python script wired into `validate-quality.yml`'s "Database access check" step): catches the patterns forbidigo can miss or that survive a linter version bump:
1. **No `database/sql` import** in production code — grep for `"database/sql"`.
2. **No direct `pgxpool.New/Connect`** — regex scan of production `.go` source text.
3. **No `pgx.Connect(Config)`** — same scan.
4. **No bare `pgcommon.Config{DSN:…}` struct literal** outside `postgres/db.go` — the system pool MUST use `pgadapter.SystemPoolConfig(dsn, log)` which forces `PGBouncerMode=true`, clears `GUCProvider`, and inherits pool sizing from `ConfigFromEnv`; a bare literal silently drops `PGBouncerMode`, causing "prepared statement already exists" under PgBouncer transaction pooling.
5. **Every `pgcommon.RunInTx*` call in `db.go` is wrapped by `wrapConnErr`** — ensures SQLSTATE connectivity/resource classes (08, 53, 57, 58) become `domain.ErrDBUnavailable` (503) and deadlock/serialization failures become `domain.ErrConflict` (409 concurrent_update) so handlers never receive raw `*pgconn.PgError` shapes.

`pgx.Tx` appears in `port/tx_runner.go` (stored in context by `WithTx`/`TxFromContext`) and in `eventbus/publisher.go` (passed to `outbox.EnqueueOrdered`). This is intentional: platform-events' outbox API requires a `pgx.Tx` parameter — there is no pgcommon-owned wrapper for it. The tx itself is ALWAYS created by `pgcommon.RunInTxWithRetryOpts` in `postgres/db.go`; the port package only stores and retrieves it so the eventbus adapter can enqueue events in the same transaction without creating a cross-adapter dependency (which arch-lint forbids).

## Events / Outbox / Dedup Rule

All three surfaces go through `platform-events` (and, for consumer-side dedup, this service's own `processed_events`/IDEMP-1..4 — see below) with no hand-rolled equivalent:

- **Publish** — `RoutingPublisher` (`internal/adapter/outbound/eventbus/routing_publisher.go`) holds two `events.Publisher` fields, each built via `events.NewSNSPublisher(eventcfg.SNSConfigFromEnv(...), events.WithCodec(codec))` (`cmd/server/wiring.go`'s `buildTopicPublisher`) — no direct `aws-sdk-go-v2/service/sns` calls anywhere in this repo (`cmd/server/main.go`'s `aws-sdk-go-v2/service/sqs` import constructs the raw client `events.NewSQSConsumerWithClient` requires as a dependency-injection parameter, and `events.NewSQSDLQPublisherWithClient` uses the same client for the DLQ forwards, below — the library builds its own SNS client internally, hence "deliberately no `sns.NewFromConfig` call in this file", per that file's own comment).
- **Enqueue** — `Publisher.Enqueue` (`internal/adapter/outbound/eventbus/publisher.go`) writes to `outbox_events` via the library's own `outbox.EnqueueOrdered(ctx, tx, env, "<tenant_id>/<subject>")` (or `outbox.Enqueue` for an event with no subject) — never a hand-rolled `INSERT`. Before an ordered enqueue the transaction takes `pg_advisory_xact_lock(hashtextextended('outbox-order:<key>', 0))`, so two transactions for one key never interleave (the library's precondition; an opposite-order deadlock is retried as 40P01) and one user's events publish in commit order. Delivery is not ordered end to end (SNS/SQS), so consumers keep their recency guards. `outbox_events` keeps the library's schema (jsonb payload); the v1-era TEXT column and hex-decode trigger are gone.
- **Consume** — every SQS consumer is built via `events.NewSQSConsumerWithClient` (`sqspipeline.NewConsumer`, always with `events.WithConsumerCodec(events.GlueDecodeCodec{})`); no hand-rolled `ReceiveMessage`/`DeleteMessage` polling loop anywhere. Each queue's handler is `sqspipeline.Handler` (`internal/adapter/inbound/sqspipeline/inbound_schema.go`): DLQ router → consumer span + queue context → consumed-schema validation (`validateConsumed`) → `Handle` (platform-events' consumer counts every delivery in `platform_messages_*` itself). DLQ handling is platform-events v2's own: every consumer gets `events.WithDLQForwarding(dlqPublisher)` (an `events.NewSQSDLQPublisherWithClient`), so undecodable bodies and messages that reach `SQS_MAX_RECEIVE_COUNT` (default 4, one below the RedrivePolicy's 5) are forwarded by the library and counted once on `platform_dlq_messages_total`. `internal/adapter/inbound/sqspipeline/dlq.go` adds only the permanent-reject shortcut: on EVT-15's `ErrPoisonPill` or a consumed-schema violation, `routeRejectsToDLQ` forwards the original message (`events.SourceMessageFromContext`) via `SendToDLQ` and acks; if that send fails the error is returned so the message stays on the queue. At startup `sqspipeline.RequireDLQ` resolves each queue's DLQ: a missing/invalid RedrivePolicy is fatal, a transient lookup failure only warns.
- **Prune** — `outbox.Runner.PrunePublished` (`cmd/reconciler/jobs/outbox_prune.go`, called until a batch comes back short — one call deletes at most `RECONCILER_BATCH_LIMIT` rows) is the *only* code that deletes from `outbox_events`; `inbox.Store.Prune` (`processed_events_prune.go` via `port.InboxPruner`, once per consumer name) the only code that deletes from `processed_events`.
- **Consumer-side dedup (`processed_events`, IDEMP-1..4)** — platform-events v2's `pkg/inbox`. `postgres.InboxRepository` (`port.Inbox`) wraps `inbox.Store.Process`: the claim (`INSERT … ON CONFLICT DO NOTHING`) is the first statement of the one transaction that also runs the consumer's projection, EVT-14 row lock and relay enqueues (fn's ctx carries the tx and the tx-bound publisher), so a concurrent duplicate waits on the claim and is then skipped — the old probe-then-insert left that race open. The library also counts `platform_duplicate_messages_total`. CI check #4 rejects hand-written SQL against `processed_events`.

**Enforced in CI via two complementary scripts:**

**`check-forbidden-events-bypass.sh`** (added 2026-09-21, ported from `iam-delegation`) — catches WHAT is used: (1) `aws-sdk-go-v2/service/sns`/`service/sqs` imports only in `cmd/server/main.go`; (2) no SNS/SQS transport method calls outside `eventbus/` (RoutingPublisher exempt — it calls `events.Publisher` interface methods, same names, not the SDK); (3) `events.Envelope{...}` struct literal never published (one permitted: the `{ID, Type}` claim key in `idempotency_repository.go`); (4) no hand-written SQL against `processed_events`.

**`check-events-platform.sh`** (added 2026-10-13) — catches WHERE platform-events' own abstractions are called (structural isolation, 4 rules):
1. **`outbox.Enqueue` / `outbox.EnqueueOrdered` only in `eventbus/publisher.go`** — every service write goes through `port.EventPublisher → eventbus.Publisher.Enqueue`, which validates the schema, takes the ordering advisory lock, and stores plain JSON. A call site elsewhere bypasses all three simultaneously.
2. **`inbox.NewStore` only in `postgres/idempotency_repository.go`** — a second store would create a siloed dedup ledger with no guarantee of consistent consumer naming or tx scoping.
3. **`inbox.Store.Process` only in `postgres/idempotency_repository.go`** — the repository wraps it with `port.WithTx + port.WithEventPublisher` injection so handlers get both the DB tx and the tx-bound publisher atomically; a raw call elsewhere skips that wiring.
4. **`events.InitMetrics` present in both `cmd/server/main.go` and `cmd/reconciler/main.go`** — platform-events metrics must be initialised once per binary; a missing call silences the entire `platform_messages_*` / `platform_dlq_*` / `platform_duplicate_*` series for that binary.

`check-outbox-access.sh` rejects hand-written SQL against `outbox_events`/`outbox_dead_letters`. All four scripts run in `validate-quality.yml`'s "Validate / Quality" job.

## Dependency Rules (enforced in CI via `go-arch-lint`, see `.go-arch-lint.yml`)

- `core/domain` — imports **nothing** outside itself (`anyVendorDeps: true`, no internal deps)
- `core/port` — imports only `core/domain`
- `core/service` — imports only `core/domain`, `core/port`, `pkg/requestctx`, and the `observability` metrics package (see below)
- `adapter/*` implements `core/port`; **nothing in `core/` imports `adapter/`**
- `internal/adapter/outbound/metrics` is its own cross-cutting **`observability`** component (collectors built with platform-gincommon's constructors, no raw `prometheus.New*`; no internal deps). `.go-arch-lint.yml` lists it as importable by `adapters_inbound`, `inbound_pipeline`, `adapters_outbound`, `reconciler_jobs`, and the composition root `cmd` — and its own `service` component's `mayDependOn` list **does** explicitly enumerate `observability` (confirmed directly against `.go-arch-lint.yml`, not just inferred), which is why `core/service` importing it directly in several files (`membership_service.go`, `dept_membership_service.go`, `invitation_service.go`, `catalog_service.go`, `provisioning_service.go`, `group_mapping_service.go`) to record business-outcome counters at the point of the outcome is a declared, sanctioned dependency, not a lint gap.
- `eventschema` (`internal/adapter/outbound/eventbus/schemas`) and `apispec` (`api/`, embedding `api/asyncapi.yaml`) are standalone `anyVendorDeps: true` leaves — `eventbus` may depend on `eventschema`; `adapters_inbound` may depend on `apispec` (`docs.go`/`asyncapi.go` serve the embedded spec directly).
- `adapters_outbound` (`postgres`, `valkey`, `eventbus`, `workflow`, `realmprovisioner`, `catalogadmin`, `groupmappingclient`, `delegationcheck`, `tokenservice`) — no `userprofile` component any more, and no delegation/tender-ACL adapters. `eventbus/publisher.go` used to import `postgres` directly (for `pgadapter.TxFromContext`) — a cross-adapter dependency `adapters_outbound`'s own `mayDependOn` list forbids. Fixed by moving `WithTx`/`TxFromContext` (and the underlying context key) into `internal/core/port/tx_runner.go`; `postgres.WithTx`/`postgres.TxFromContext` remain as thin re-export wrappers for existing test call sites. The fix is committed; only `eventbus/publisher_test.go` still imports `postgres` (tests are excluded from arch-lint).
- `httptransport` (`internal/adapter/outbound/httpx`) — a shared, otelhttp-instrumented `http.Client` factory (`httpx.NewClient(timeout)`), replacing every outbound client's former manual `http.Client` construction + per-package `propagateTraceparent` helper. **Adopted**: verified by direct grep, all six outbound clients (`workflow`, `realmprovisioner`, `catalogadmin`, `groupmappingclient`, `delegationcheck`, `tokenservice`) build their client via `httpx.NewClient(timeout)`; zero leftover manual `traceparent`/`propagateTraceparent` code anywhere in that package set — `otelhttp.NewTransport` handles injection + client-span emission, and an inner round-tripper forwards the inbound `x-request-id` from `requestctx.RequestIDFromContext`. Declared in `.go-arch-lint.yml` as its own component (`anyVendorDeps: true`, may depend on `requestctx` only) so `adapters_outbound` may depend on it.
- `inbound_pipeline` (`internal/adapter/inbound/sqspipeline`) — wraps `adapters_inbound`'s consumers; may depend on `adapters_inbound`/`port`/`domain`/`observability` (`inbound_schema.go` counts schema rejects). Its own component because arch-lint does not allow one package of a component to import another. The composition root adapts `eventbus.ValidatingCodec` (`ErrNoSchema` → valid) so it never imports an outbound adapter.
- `reconciler_jobs` (`cmd/reconciler/jobs`) — one file per CronJob dispatched by `cmd/reconciler --job=<name>`; may depend on `domain`/`port`/`observability`/`requestctx`. Declared as its own component (not folded into `cmd`) so arch-lint can scope its dependency set independently of `cmd/server`'s.
- `docs_swagger` (`docs/swagger`, the `make swag`-generated output blank-imported in `cmd/server/main.go`) is a pure `anyVendorDeps: true` leaf, declared solely so arch-lint doesn't flag that import.
- `cmd` (`cmd/server`, `cmd/reconciler`, `cmd/migrate`) is the composition root; may import everything else (`domain`, `port`, `service`, `requestctx`, `eventschema`, `observability`, `adapters_inbound`, `inbound_pipeline`, `adapters_outbound`, `httptransport`, `reconciler_jobs`, `docs_swagger`) — not `apispec`, which only `adapters_inbound` serves
