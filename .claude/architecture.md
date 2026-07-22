# Architecture

Refines HLD §5.6, §15.3. Clean Architecture / Ports-and-Adapters — dependencies point inward; `core/domain` imports nothing external; `cmd/server/main.go` is the only composition root.

## Package Layout

```
iam-org-membership/
├── cmd/
│   └── server/
│       └── main.go                     # composition root, pool + middleware wiring, graceful shutdown
├── internal/
│   ├── core/
│   │   ├── domain/                     # entities, value objects, DomainError; NO external deps
│   │   │   ├── tenant.go               # Tenant, TenantPlan, SubscriptionStatus
│   │   │   ├── department.go           # Department, DepartmentID
│   │   │   ├── membership.go           # TenantMembership, DeptMembership, RoleLevel
│   │   │   ├── role.go                 # TenantRole, RoleCode
│   │   │   ├── group_mapping.go        # GroupRoleMapping, GroupDeptMapping
│   │   │   ├── delegation.go           # Delegation, DelegationScope
│   │   │   ├── acl.go                  # TenderACLEntry
│   │   │   ├── events.go               # DomainEvent payload types
│   │   │   └── errors.go               # ErrNotFound, ErrConflict, ErrWorkflowResolutionRequired, ErrSeatLimitReached, ErrInvitationAlreadyExists, ErrInvalidReplacement
│   │   ├── port/                       # interfaces required by the core
│   │   │   ├── tenant_repository.go
│   │   │   ├── department_repository.go
│   │   │   ├── membership_repository.go
│   │   │   ├── role_repository.go
│   │   │   ├── group_mapping_repository.go
│   │   │   ├── delegation_repository.go
│   │   │   ├── acl_repository.go
│   │   │   ├── cache.go                # Valkey interface (advisory)
│   │   │   ├── user_profile_client.go  # SetAvailability (delegation OOO coordination)
│   │   │   ├── workflow_client.go      # (NEW §8.8) GetDelegateImpact, ReassignDelegate, CancelByDelegate
│   │   │   ├── realm_provisioner_client.go # CreateInvitedUser, DeleteUser, PatchRealmConfig, RevokeUserSessions
│   │   │   └── event_publisher.go
│   │   └── service/                    # use cases
│   │       ├── tenant_service.go
│   │       ├── department_service.go
│   │       ├── membership_service.go   # (amended) RemoveUser — delegate-impact pre-check + resolution (§8.8); Invite (§8.10); AcceptInvitation (I-3 branch)
│   │       ├── role_service.go
│   │       ├── group_mapping_service.go
│   │       ├── delegation_service.go
│   │       └── acl_service.go
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                   # Gin handlers, DTOs
│       │   │   ├── tenant_handler.go
│       │   │   ├── department_handler.go
│       │   │   ├── membership_handler.go # DELETE pre-check; POST .../removal-resolution (P-26); invite lifecycle
│       │   │   ├── role_handler.go
│       │   │   ├── group_mapping_handler.go
│       │   │   ├── delegation_handler.go
│       │   │   ├── acl_handler.go
│       │   │   └── dto.go              # DelegateImpact / RemovalResolution / SeatUsage / Invitation DTOs
│       │   └── consumer/
│       │       └── membership_event_consumer.go # SQS handler: iam.tenant.events, billing.events (EVT-14 recency, EVT-15 clamp, EVT-16 relay)
│       └── outbound/
│           ├── postgres/               # repository impls + migrations/
│           ├── valkey/                 # cache impl (go-redis/v9)
│           ├── eventbus/               # RoutingPublisher (2 topics) + outbox runner
│           ├── userprofile/            # HTTP client for User Profile (http_client.go)
│           ├── workflow/               # (NEW) HTTP client for Workflow Service
│           └── realmprovisioner/       # HTTP client for Realm Provisioner
├── api/
│   ├── openapi.yaml                    # REST contract (all P-*/I-*/O-* routes)
│   └── asyncapi.yaml                   # AsyncAPI 3.0 — iam.membership.events + iam.tenant.events channels (design-time source of truth)
├── internal/eventschema/               # JSON Schema Draft-07, ONE FILE per event type; derived via `schema-gov extract`, committed
│   ├── DepartmentMembershipGranted.json
│   ├── DepartmentMembershipRevoked.json
│   ├── DepartmentMembershipLevelChanged.json
│   ├── TenantRoleGranted.json
│   ├── TenantRoleRevoked.json          # (NEW, §16 A14) multi-role support
│   ├── DelegationStarted.json
│   ├── DelegationEnded.json            # (extended, DEL-7) new ended_reason='delegate_removed'
│   ├── TenderAssigneeOverridden.json
│   ├── TenantCreated.json
│   └── TrialStarted.json
├── docs/
│   ├── schema-archive/                 # archived Glue versions (schema-gov prune --mode archive)
│   └── schema-changelog.md             # schema-gov changelog output
├── deploy/helm/                        # Helm chart (11 CronJobs — see operations.md)
├── test/{postgres,integration,e2e,smoke,fixtures}/
├── Dockerfile  docker-compose.yml  Makefile  go.mod  .golangci.yml
```

`internal/eventschema/` **is the schema-governance workspace.** `schema-gov extract` generates the Draft-07 files from `asyncapi.yaml`; `extract --check` runs in CI to detect drift; `schema-gov register` uploads to AWS Glue Schema Registry.

## Shared Library Dependencies (HLD §15.4)

```go
require (
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon    v1.2.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events        v1.3.0
    github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon      v1.1.1
)
```

**Not a Go module:** `platform-schemagov` is a Python 3.12 CLI (`schema-gov`) shipped as `ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:v0.3.0`. Invoked via `docker run -v $PWD:/workspace` in CI only — zero Go-code presence.

**Not a dependency:** `iam-keycloakclient` — this service **never calls the Keycloak Admin API**; that is Realm Provisioner's exclusive responsibility (HLD §4.2).

### `platform-gincommon` integration

Middleware stack identical to `iam-user-profile`: `PanicRecovery → RequestID → Tracing → CorrelationHeaders → Metrics → Logging → RequireAuth → ContextMiddleware`, plus the GUC-bridge middleware. `ServiceName = "iam-org-membership"`. **No gRPC server** — Gin HTTP only. `TimeoutMiddleware`, `DefaultMiddlewares`, `HealthHandler`, `RequestContext`, `ErrorResponse`, `InitTracingFromEnv`, `Shutdown`. `gincommon.PropagateHeaders` carries W3C `traceparent` to every outbound client (User Profile, Workflow, Realm Provisioner).

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

## Dependency Rules (enforced in CI via `go-arch-lint`)

- `core/domain` — imports **nothing** outside itself
- `core/port` — imports only `core/domain`
- `core/service` — imports only `core/domain` and `core/port`
- `adapter/*` implements `core/port`; **nothing in `core/` imports `adapter/`**
