# Architecture

Refines HLD §5.6, §15.3. Clean Architecture / Ports-and-Adapters — dependencies point inward; `core/domain` imports nothing external; `cmd/server/main.go` (HTTP server) and `cmd/reconciler/main.go` (CronJob dispatcher, `--job=<name>`) are the two composition roots.

## Package Layout

```
iam-org-membership/
├── cmd/
│   ├── server/
│   │   └── main.go                     # HTTP composition root, pool + middleware wiring, graceful shutdown
│   └── reconciler/
│       ├── main.go                     # CronJob dispatcher, --job=<name> flag
│       └── jobs/                       # invitation-expiry, invitation-kc-cleanup, realm-config-sync, seat-overage-reconcile, trial-cleanup, outbox-prune, processed-events-prune (7 jobs; delegation-expiry/delegation-review removed)
├── internal/
│   ├── core/
│   │   ├── domain/                     # entities, value objects, DomainError; NO external deps
│   │   │   ├── tenant.go               # Tenant, TenantPlan, SubscriptionStatus
│   │   │   ├── department.go           # Department, TenantDepartment
│   │   │   ├── membership.go           # TenantMembership, DeptMembershipView, MembershipListPage
│   │   │   ├── role.go                 # TenantRole, TenantRoleCode, DeptMembership, DeptRoleLabel
│   │   │   ├── group_mapping.go        # GroupDeptRoleMapping, GroupTenantRoleMapping, GroupDeptMapping
│   │   │   ├── plan.go                 # Plan, PlanPatch, BrandingLevel — client-side cache representation; Catalog Service owns the table
│   │   │   ├── invitation.go           # PendingInvitation, InvitationDeptMapping, SeatUsage
│   │   │   ├── event.go                # DomainEvent envelope
│   │   │   ├── event_payloads.go       # per-event payload structs
│   │   │   └── errors.go               # ErrNotFound, ErrConflict, ErrWorkflowResolutionRequired, ErrSeatLimitReached, ErrInvitationAlreadyExists, ErrInvalidReplacement
│   │   │                               # NO delegation.go, NO acl.go — moved to Delegation Service / Tender ACL Service
│   │   ├── port/                       # interfaces required by the core
│   │   │   ├── tenant_repository.go
│   │   │   ├── authz_repository.go     # NEW. AuthZRepository — the I-8 four-table join, moved out of authz_service.go so core/service depends on a port, not *pgcommon.Pool directly
│   │   │   ├── department_repository.go # TenantDepartmentRepository — per-tenant activation junction only; the global catalog itself is external
│   │   │   ├── membership_repository.go # MembershipRepository, TenantRoleRepository, DeptMembershipRepository, DeptRoleLabelRepository
│   │   │   ├── invitation_repository.go
│   │   │   ├── catalog_reader.go       # NEW (ADR-0007). DepartmentCatalogReader, PlanCatalogReader — narrow read interfaces backing CatalogService
│   │   │   ├── group_mapping_client.go # NEW (ADR-0007). GroupMappingClient — replaces the old local group_mapping_repository.go
│   │   │   ├── outbound_clients.go     # WorkflowClient (§8.8), RealmProvisionerClient, CatalogAdminClient (NEW), DelegationCheckClient (NEW, ADR-0008) — NO UserProfileClient
│   │   │   ├── cache.go                # Valkey interface (advisory)
│   │   │   ├── event_publisher.go
│   │   │   └── tx_runner.go
│   │   │                               # NO delegation_repository.go, NO acl_repository.go, NO user_profile_client.go, NO standalone workflow_client.go/realm_provisioner_client.go (folded into outbound_clients.go)
│   │   └── service/                    # use cases
│   │       ├── tenant_service.go
│   │       ├── department_service.go
│   │       ├── membership_service.go   # RemoveUser — delegate-impact pre-check + resolution (§8.8); Invite (§8.10); AcceptInvitation (I-3 branch); tenant_roles cascade (TR-9)
│   │       ├── dept_membership_service.go # dept-level assign/remove gating; calls DelegationCheckClient for dept-scoped precision, degrades to tenant-wide on outage
│   │       ├── invitation_service.go
│   │       ├── role_label_service.go
│   │       ├── provisioning_service.go
│   │       ├── operator_service.go
│   │       ├── authz_service.go        # I-8 hot path; depends on port.AuthZRepository (NEW), not *pgcommon.Pool — the four-table join itself lives in the postgres adapter, this owns TR-7/PLAN-6 composition
│   │       ├── catalog_service.go      # NEW (ADR-0007). Read-through + stale-if-error cache over CatalogAdminClient — NOT a table owner
│   │       ├── group_mapping_service.go # client-side resolve over GroupMappingClient — NOT a table owner, fails open
│   │       └── subscription_lapse_service.go # NEW (I-16, §16 OQ-9/RP-C3). Cross-tenant read; its TenantRepository MUST be sysPool-bound (BYPASSRLS), not the RLS-scoped app pool
│   │                                   # NO delegation_service.go, NO acl_service.go, NO role_service.go (renamed role_label_service.go — tenant_roles grants now live in membership_service.go)
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                   # Gin handlers, DTOs
│       │   │   ├── router.go           # single source of truth for the route table, shared by cmd/server/main.go and the e2e test harness
│       │   │   ├── tenant_handler.go
│       │   │   ├── department_handler.go
│       │   │   ├── membership_handler.go # DELETE pre-check; POST .../removal-resolution (P-26); invite lifecycle
│       │   │   ├── dept_membership_handler.go
│       │   │   ├── role_label_handler.go
│       │   │   ├── invitation_handler.go
│       │   │   ├── operator_handler.go
│       │   │   ├── internal_handler.go # I-1..I-16, incl. I-15 GET .../members/:user_id/exists (grant-time check for Delegation/Tender-ACL services) and NEW I-16 GET /subscription-lapses (RP-C3 bulk read)
│       │   │   ├── middleware.go
│       │   │   └── dto.go
│       │   │                           # NO delegation_handler.go, NO acl_handler.go, NO group_mapping_handler.go (admin group-mapping CRUD moved to Group Mapping Service)
│       │   └── consumer/
│       │       ├── membership_event_consumer.go # SQS handler: iam.tenant.events, billing.events (EVT-14 recency, EVT-15 clamp, EVT-16 relay)
│       │       ├── dedup.go            # skipDuplicate/ackUnknown/markProcessedInTx — shared IDEMP-1..4 helpers, used by both consumers below
│       │       └── catalog_consumer.go # SQS handler: catalog-orgm-q ← DepartmentCatalogChanged (Gap 12 org-membership half) — clears om:departments/om:departments:stale; now unit-tested and uses the shared dedup.go helpers (previously ad hoc); still inert until iam-catalog-admin's publisher side + SQS_CATALOG_ORGM_QUEUE_URL exist
│       └── outbound/
│           ├── postgres/               # repository impls + migrations/ (single consolidated 000000_initial_schema — never deployed, so the incremental decomposition history was squashed)
│           ├── valkey/                 # cache impl (go-redis/v9)
│           ├── eventbus/               # RoutingPublisher (2 topics) + outbox runner; schemas/*.json embedded via ValidatingCodec
│           ├── metrics/                # cross-cutting Prometheus counters/histograms (business.go) — the "observability" component in .go-arch-lint.yml
│           ├── workflow/               # HTTP client for Workflow Service — retained, unrelated to the delegation extraction
│           ├── realmprovisioner/       # HTTP client for Realm Provisioner
│           ├── catalogadmin/           # NEW (ADR-0007). HTTP client for Catalog / Admin Config Service (departments, plans — read-only)
│           ├── groupmappingclient/     # NEW (ADR-0007). HTTP client for Group Mapping / JIT Config Service
│           └── delegationcheck/        # NEW (ADR-0008). HTTP client for Delegation Service's dept-delegate lookup
│                                       # NO userprofile/ package — dead code once delegation's OOO coordination moved out, deleted (LLD §16 OQ-5)
├── api/
│   └── asyncapi.yaml                   # AsyncAPI 3.0 — iam.membership.events + iam.tenant.events channels (design-time source of truth)
│                                       # openapi.yaml DELETED — REST contract is now docs/swagger/* (generated via `make swag` from handler annotations), not hand-authored
├── docs/
│   ├── swagger/                        # generated by `make swag` — swagger.yaml/swagger.json/docs.go; regenerated in this pass to drop retired routes/DTOs
│   ├── schema-archive/                 # archived Glue versions (schema-gov prune --mode archive)
│   └── schema-changelog.md             # schema-gov changelog output
├── deploy/helm/                        # Helm chart (7 CronJobs — see operations.md; delegation-expiry/delegation-review/delegation-cleanup/acl-cleanup removed)
├── test/{unit,postgres,integration,e2e}/
├── Dockerfile  docker-compose.yml  Makefile  go.mod  .golangci.yml
```

`internal/adapter/outbound/eventbus/schemas/` **is the one schema-governance workspace** — there is no separate `internal/eventschema/` directory. `schema-gov extract --schema-dir internal/adapter/outbound/eventbus/schemas` generates the Draft-07 files directly from `asyncapi.yaml` into this location, which doubles as the runtime copy embedded via `//go:embed schemas/*.json` in the `ValidatingCodec`; `.github/workflows/schema-registry.yml` validates/diffs/registers against this same path; `schema-gov register` uploads to AWS Glue Schema Registry. There is no separate design-time-vs-runtime split.

## Shared Library Dependencies (HLD §15.4)

```go
require (
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon    v1.3.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events        v1.4.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon      v1.3.0
)
```

**Not a Go module:** `platform-schemagov` is a Python 3.12 CLI (`schema-gov`) shipped as `ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4`. Invoked via `docker run -v $PWD:/workspace` in CI only — zero Go-code presence.

**Not a dependency:** `iam-keycloakclient` — this service **never calls the Keycloak Admin API**; that is Realm Provisioner's exclusive responsibility (HLD §4.2).

### `platform-gincommon` integration

`router.go` (`NewRouter`) chains, in order: a 1 MB body cap → `gincommon.TimeoutMiddleware(30s)` → `gincommon.ObservabilityMiddlewares` (`PanicRecovery → RequestID → Tracing → CorrelationHeaders → Metrics → Logging`) → `NormalizeAuthErrors()` (G-13, adds a `code` field to gincommon's bare 401 body) — then, per `/api/v1` sub-group, `gincommon.ProtectedMiddlewares` (`RequireAuth → ContextMiddleware`) → this repo's own `GUCBridgeMiddleware` → `RequireJSONContentType`, followed by route-specific gates (`RequireActiveTenant`/`RequireActiveMembership` on public `/tenants/*`, `RequireOperatorRole` on `/operator/*`, `RequireSystemRole` on `/internal/*`). `ServiceName = "iam-org-membership"`. **No gRPC server** — Gin HTTP only. Each of the six outbound clients (Workflow, Realm Provisioner, Catalog Admin, Group Mapping, Delegation Check, Token Service — no User Profile client any more, that adapter was deleted as dead code) builds its `http.Client` via `httpx.NewClient(timeout)`, whose `otelhttp`-wrapped transport injects W3C `traceparent` and emits a client span automatically — not a hand-rolled `propagateTraceparent` helper, and not `gincommon.PropagateHeaders` either (that's the inbound-focused helper; this is the outbound leg). (A prior revision of this doc described a manual per-package `propagateTraceparent` helper as the current mechanism and `httpx` as unadopted scaffolding — verified stale via direct grep, corrected here.)

### `platform-pgcommon` integration

- `NewPool` + `ConfigFromEnv`, `PGBouncerMode` env-driven.
- **`GUCProvider = GUCSetFromContext`** — binds `app.tenant_id` **transaction-locally** via `set_config(..., is_local => true)` on **every** checkout, reads included. Never a session-scoped `SET` (would persist on a pooled backend and leak across tenants; RLS-6). This is the security-critical invariant that makes RLS safe under PgBouncer transaction pooling.
- `RunInTx` for all writes AND all reads — no bare non-transactional query path exists.
- `RunInSavepoint` for sub-step rollbacks.
- Error mapping: `IsUniqueViolation` / `IsForeignKeyViolation` / `IsCheckViolation` / `IsDeadlock` / `IsSerializationFailure`.
- `Pool.Health`, `migrate.Runner`, `pgmetrics.Init`, `NewOTelQueryTracer`, `SlowQueryTracer`.
- **`Config.SlowQueryThreshold = 200 ms`** (WARN log with `tenant_id` redacted).
- Production: `SetLogTenantID(false)`, `SetAllowFullStatements(false)`.

### `platform-events` integration — RoutingPublisher

Unlike `iam-user-profile` (single-topic producer), O&M wires its own local **`eventbus.RoutingPublisher`** (HLD §9.2) — not a `platform-events` type; that library only exposes `NewSNSPublisher` for a single topic. It holds two pre-built `events.Publisher` fields (`membership`, `tenant`), not a topic-ARN map, and routes each envelope via **`domain.TopicForEvent(env.Type)`** — keyed on event **type**, not `Envelope.Source`: `TenantCreated`/`TrialStarted` → `tenant`, everything else → `membership`. This is the one meaningful wiring difference from User Profile.

**Substantive consumer of two topics** at MVP:
- **`iam.tenant.events`** via `tenant-orgm-q` — Realm Provisioner tenant-lifecycle events (`TrialTenantProvisioned`, `TenantRealmReady`, `TenantConverted`, `DirectPaidSignup`, `TrialExpired`, `TrialReactivated`, `TenantSuspended`, `TenantOffboarded`, `TenantReactivated{source=operator}` — new, resolves F1 of the RP↔O&M alignment review, RP-10 reversing RP-14). O&M **never self-consumes** its own `TenantCreated`/`TrialStarted` (HLD §9.1.1 "No self-consumption" — produce/consume sets are disjoint).
- **`billing.events`** via `billing-orgm-q` — Billing Service events (`TenantPlanChanged`, `TenantPaymentPastDue`, `TenantSubscriptionCancelled`, `TenantReactivated`, `TenantSeatsChanged`).

Queue naming: `<topic-short>-<consumer-short>-q` (HLD §9.1). Consumer short-name `orgm`. Both DLQs `tenant-orgm-q-dlq` / `billing-orgm-q-dlq`, `maxReceiveCount = 5`. Idempotency via `processed_events`.

## Database Access Rule (enforced in CI via `golangci-lint`'s `forbidigo`, see `.golangci.yml`)

Every DB connection/pool — production and test alike — must go through `pgcommon.NewPool` (`internal/adapter/outbound/postgres/db.go`, `test/dbseed/pool.go`), never a raw `pgxpool.New`/`pgxpool.Connect`/`pgx.Connect`/`database/sql.Open`. This is what makes GUC binding (RLS-6), retry-on-deadlock, slow-query logging, and OTel `db.query` spans structurally inescapable rather than a convention someone could quietly bypass. Added 2026-09-20 as a `forbidigo` rule (`analyze-types: true`, matching the resolved qualified identifier — e.g. `pgxpool.New` — not call-site text, so a trailing `\(` in the pattern would silently match nothing; verified against a deliberate sanity-check violation before trusting the rule). Two files legitimately import `pgxpool` without tripping this — `test/dbseed/pool.go` (wraps `pgcommon.Pool`, only needs the `*pgxpool.Conn` parameter type) and `test/e2e/security_test.go` (calls `pgcommon.Pool.WithConn`, same reason) — `forbidigo` bans the construction *functions*, not the *types*, so both pass cleanly. Verified zero violations repo-wide (production and test) before and after adding the rule.

## Events / Outbox / Dedup Rule

All three surfaces go through `platform-events` (and, for consumer-side dedup, this service's own `processed_events`/IDEMP-1..4 — see below) with no hand-rolled equivalent:

- **Publish** — `RoutingPublisher` (`internal/adapter/outbound/eventbus/routing_publisher.go`) holds two `events.Publisher` fields, each built via `events.NewSNSPublisher(eventcfg.SNSConfigFromEnv(...), events.WithCodec(codec))` (`cmd/server/wiring.go`'s `buildTopicPublisher`) — no direct `aws-sdk-go-v2/service/sns` calls anywhere in this repo (`cmd/server/main.go`'s one `aws-sdk-go-v2` import is `service/sqs`, to construct the raw client `events.NewSQSConsumerWithClient` requires as a dependency-injection parameter — the library builds its own SNS client internally, hence "deliberately no `sns.NewFromConfig` call in this file", per that file's own comment).
- **Enqueue** — `Publisher.Enqueue` (`internal/adapter/outbound/eventbus/publisher.go`) writes to `outbox_events` via `outbox.Enqueue(ctx, tx, env)` — the library's own package-level function — never a hand-rolled `INSERT`.
- **Consume** — every SQS consumer is built via `events.NewSQSConsumerWithClient` (`cmd/server/wiring.go`'s `buildSQSConsumer`); no hand-rolled `ReceiveMessage`/`DeleteMessage` polling loop anywhere.
- **Prune** — `outbox.Runner.PrunePublished` (`cmd/reconciler/jobs/outbox_prune.go`) is the sanctioned path for deleting from `outbox_events`. A hand-rolled equivalent, `port.ReconcilerStore.PruneOutbox` (`internal/adapter/outbound/postgres/reconciler_store.go`), still exists as of 2026-09-20 — confirmed to have zero production callers once `outbox_prune.go`'s migration to `PrunePublished` lands, but its removal is entangled with that in-flight migration (`cmd/reconciler/jobs/context.go`'s `Context.OutboxRunner` wiring) and is **not yet done**; a CI enforcement script (`.github/scripts/check-outbox-access.sh`, not yet wired into `validate-quality.yml`) exists on disk to catch this class of violation once the removal lands, but wiring it in now would fail CI against the still-present `PruneOutbox` hand-rolled SQL.
- **Consumer-side dedup (`processed_events`, IDEMP-1..4)** — deliberately **not** migrated to anything, because `platform-events` has no consumer-side idempotency/dedup store to migrate to (checked directly: only publish-side SNS FIFO `MessageDeduplicationID` support exists in the library, nothing consumer-facing). `processed_events` + `internal/adapter/inbound/consumer/dedup.go`'s `skipDuplicate`/`markProcessedInTx` remain a legitimately locally-owned concept, same as `PruneProcessedEvents` (`ReconcilerStore`) — not a bypass of anything the library provides.

**Enforced in CI** (added 2026-09-21, `.github/scripts/check-forbidden-events-bypass.sh`, wired into `validate-quality.yml`, ported from `iam-delegation`'s identically-named script): (1) `aws-sdk-go-v2/service/sns`/`service/sqs` may only be imported in `cmd/server/main.go` (the one place allowed to construct a raw `*sqs.Client`, solely to hand it to `events.NewSQSConsumerWithClient`); (2) outside `internal/adapter/outbound/eventbus/`, no code may call an SNS/SQS transport method (`Publish`/`SendMessage`/`ReceiveMessage`/`DeleteMessage`/`ChangeMessageVisibility`/`Subscribe`) directly — `eventbus/` itself is exempt from this one check because `RoutingPublisher.Publish`/`PublishBatch` legitimately call through to the wrapped `events.Publisher` interface, which happens to share method names with the raw SDK; (3) `events.Envelope{...}` may never be constructed as a struct literal, only via `events.NewEnvelope(...)`. Verified against three deliberate sanity-check violations (one per rule, created, confirmed each fired, cleaned up) before trusting it, and re-verified zero false positives against the real repo, including the `eventbus/` exemption's one otherwise-matching call site (`routing_publisher.go`'s `pub.Publish(ctx, env)`).

## Dependency Rules (enforced in CI via `go-arch-lint`, see `.go-arch-lint.yml`)

- `core/domain` — imports **nothing** outside itself (`anyVendorDeps: true`, no internal deps)
- `core/port` — imports only `core/domain`
- `core/service` — imports only `core/domain`, `core/port`, and `pkg/requestctx`
- `adapter/*` implements `core/port`; **nothing in `core/` imports `adapter/`**
- `internal/adapter/outbound/metrics` is its own cross-cutting **`observability`** component (plain Prometheus instrumentation, no internal deps). `.go-arch-lint.yml` lists it as importable by `adapters_inbound`, `adapters_outbound`, and the composition root `cmd` — and its own `service` component's `mayDependOn` list **does** explicitly enumerate `observability` (confirmed directly against `.go-arch-lint.yml`, not just inferred), which is why `core/service` importing it directly in several files (`membership_service.go`, `catalog_service.go`, `provisioning_service.go`, `group_mapping_service.go`) to record business-outcome counters at the point of the outcome is a declared, sanctioned dependency, not a lint gap.
- `eventschema` (`internal/adapter/outbound/eventbus/schemas`) and `apispec` (`api/`, embedding `api/asyncapi.yaml`) are standalone `anyVendorDeps: true` leaves — `eventbus` may depend on `eventschema`; `adapters_inbound` may depend on `apispec` (`docs.go`/`asyncapi.go` serve the embedded spec directly).
- `adapters_outbound` (`postgres`, `valkey`, `eventbus`, `workflow`, `realmprovisioner`, `catalogadmin`, `groupmappingclient`, `delegationcheck`, `tokenservice`) — no `userprofile` component any more, and no delegation/tender-ACL adapters. `eventbus/publisher.go` used to import `postgres` directly (for `pgadapter.TxFromContext`) — a cross-adapter dependency `adapters_outbound`'s own `mayDependOn` list forbids. Fixed by moving `WithTx`/`TxFromContext` (and the underlying context key) into `internal/core/port/tx_runner.go`; `postgres.WithTx`/`postgres.TxFromContext` remain as thin re-export wrappers for existing test call sites. This fix has needed reapplying once already — it lives only in the working tree (never committed), and got silently reverted when unrelated external commits landed mid-session and reintroduced the direct `postgres` import. `go-arch-lint check --project-path .` currently reports **zero violations**; re-verify this specific import before assuming it still holds after the next rebase.
- `httptransport` (`internal/adapter/outbound/httpx`) — a shared, otelhttp-instrumented `http.Client` factory (`httpx.NewClient(timeout)`), replacing every outbound client's former manual `http.Client` construction + per-package `propagateTraceparent` helper. **Adopted**: verified by direct grep, all six outbound clients (`workflow`, `realmprovisioner`, `catalogadmin`, `groupmappingclient`, `delegationcheck`, `tokenservice`) build their client via `httpx.NewClient(timeout)`; zero leftover manual `traceparent`/`propagateTraceparent` code anywhere in that package set — `otelhttp.NewTransport` handles injection + client-span emission. Declared in `.go-arch-lint.yml` as its own component (`anyVendorDeps: true`) so `adapters_outbound` may depend on it.
- `reconciler_jobs` (`cmd/reconciler/jobs`) — one file per CronJob dispatched by `cmd/reconciler --job=<name>`; may depend on `domain`/`port`/`observability`/`requestctx`. Declared as its own component (not folded into `cmd`) so arch-lint can scope its dependency set independently of `cmd/server`'s.
- `docs_swagger` (`docs/swagger`, the `make swag`-generated output blank-imported in `cmd/server/main.go`) is a pure `anyVendorDeps: true` leaf, declared solely so arch-lint doesn't flag that import.
- `cmd` (`cmd/server`, `cmd/reconciler`) is the composition root; may import everything else, including `httptransport`, `reconciler_jobs`, and the generated `docs_swagger` assets
