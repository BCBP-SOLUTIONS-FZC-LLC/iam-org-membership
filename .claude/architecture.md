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
│   │   │   ├── department_repository.go # TenantDepartmentRepository — per-tenant activation junction only; the global catalog itself is external
│   │   │   ├── membership_repository.go # MembershipRepository, TenantRoleRepository, DeptMembershipRepository, DeptRoleLabelRepository
│   │   │   ├── invitation_repository.go
│   │   │   ├── catalog_reader.go       # NEW (ADR-0007). DepartmentCatalogReader, PlanCatalogReader — narrow read interfaces backing CatalogService
│   │   │   ├── group_mapping_client.go # NEW (ADR-0007). GroupMappingClient — replaces the old local group_mapping_repository.go
│   │   │   ├── outbound_clients.go     # WorkflowClient (§8.8), RealmProvisionerClient, CatalogAdminClient (NEW), DelegationCheckClient (NEW, ADR-0008) — NO UserProfileClient
│   │   │   ├── cache.go                # Valkey interface (advisory)
│   │   │   ├── event_publisher.go
│   │   │   ├── event_publisher_context.go
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
│   │       ├── authz_service.go
│   │       ├── catalog_service.go      # NEW (ADR-0007). Read-through + stale-if-error cache over CatalogAdminClient — NOT a table owner
│   │       └── group_mapping_service.go # client-side resolve over GroupMappingClient — NOT a table owner, fails open
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
│       │   │   ├── internal_handler.go # I-1..I-15, incl. NEW I-15 GET .../members/:user_id/exists (grant-time check for Delegation/Tender-ACL services)
│       │   │   ├── middleware.go
│       │   │   └── dto.go
│       │   │                           # NO delegation_handler.go, NO acl_handler.go, NO group_mapping_handler.go (admin group-mapping CRUD moved to Group Mapping Service)
│       │   └── consumer/
│       │       └── membership_event_consumer.go # SQS handler: iam.tenant.events, billing.events (EVT-14 recency, EVT-15 clamp, EVT-16 relay)
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
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon    v1.2.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events        v1.4.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon      v1.1.1
)
```

**Not a Go module:** `platform-schemagov` is a Python 3.12 CLI (`schema-gov`) shipped as `ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4`. Invoked via `docker run -v $PWD:/workspace` in CI only — zero Go-code presence.

**Not a dependency:** `iam-keycloakclient` — this service **never calls the Keycloak Admin API**; that is Realm Provisioner's exclusive responsibility (HLD §4.2).

### `platform-gincommon` integration

Middleware stack identical to `iam-user-profile`: `PanicRecovery → RequestID → Tracing → CorrelationHeaders → Metrics → Logging → RequireAuth → ContextMiddleware`, plus the GUC-bridge middleware. `ServiceName = "iam-org-membership"`. **No gRPC server** — Gin HTTP only. `TimeoutMiddleware`, `DefaultMiddlewares`, `HealthHandler`, `RequestContext`, `ErrorResponse`, `InitTracingFromEnv`, `Shutdown`. `gincommon.PropagateHeaders` carries W3C `traceparent` to every outbound client (Workflow, Realm Provisioner, Catalog Admin, Group Mapping, Delegation Check — no User Profile client any more, that adapter was deleted as dead code).

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

Unlike `iam-user-profile` (single-topic producer), O&M uses **`events.NewRoutingPublisher`** (HLD §9.2) with `TopicARNs: map[string]string{"iam.membership.events": ..., "iam.tenant.events": ...}` and a routing-key function that inspects `Envelope.Source` to select the topic ARN. This is the one meaningful wiring difference from User Profile.

**Substantive consumer of two topics** at MVP:
- **`iam.tenant.events`** via `tenant-orgm-q` — Realm Provisioner tenant-lifecycle events (`TrialTenantProvisioned`, `TenantRealmReady`, `TenantConverted`, `DirectPaidSignup`, `TrialExpired`, `TrialReactivated`, `TenantSuspended`, `TenantOffboarded`). O&M **never self-consumes** its own `TenantCreated`/`TrialStarted` (HLD §9.1.1 "No self-consumption" — produce/consume sets are disjoint).
- **`billing.events`** via `billing-orgm-q` — Billing Service events (`TenantPlanChanged`, `TenantPaymentPastDue`, `TenantSubscriptionCancelled`, `TenantReactivated`, `TenantSeatsChanged`).

Queue naming: `<topic-short>-<consumer-short>-q` (HLD §9.1). Consumer short-name `orgm`. Both DLQs `tenant-orgm-q-dlq` / `billing-orgm-q-dlq`, `maxReceiveCount = 5`. Idempotency via `processed_events`.

## Dependency Rules (enforced in CI via `go-arch-lint`, see `.go-arch-lint.yml`)

- `core/domain` — imports **nothing** outside itself (`anyVendorDeps: true`, no internal deps)
- `core/port` — imports only `core/domain`
- `core/service` — imports only `core/domain`, `core/port`, and `pkg/requestctx`
- `adapter/*` implements `core/port`; **nothing in `core/` imports `adapter/`**
- `internal/adapter/outbound/metrics` is its own cross-cutting **`observability`** component (plain Prometheus instrumentation, no internal deps). `.go-arch-lint.yml` lists it as importable by `adapters_inbound`, `adapters_outbound`, and the composition root `cmd`. In practice `core/service` also imports it directly in several files (`membership_service.go`, `catalog_service.go`, `provisioning_service.go`, `group_mapping_service.go`) to record business-outcome counters at the point of the outcome rather than only at the HTTP/consumer boundary — note the `service` component's `mayDependOn` list in `.go-arch-lint.yml` does not yet enumerate `observability`, so this is worth confirming with whoever owns the lint config.
- `adapters_outbound` (`postgres`, `valkey`, `eventbus`, `workflow`, `realmprovisioner`, `catalogadmin`, `groupmappingclient`, `delegationcheck`) — no `userprofile` component any more, and no delegation/tender-ACL adapters
- `cmd` (`cmd/server`, `cmd/reconciler`) is the composition root; may import everything else, including the generated `docs_swagger` assets
