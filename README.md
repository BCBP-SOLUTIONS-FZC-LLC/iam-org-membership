# iam-org-membership

Service that owns the **organizational layer** of the IAM subsystem — how users are grouped into tenants and departments, what roles they hold, and how tenants are structured and entitled. Consumed on every authenticated request by AuthZ Enrichment via `GET /api/v1/internal/users/:id/memberships` (I-8, LLD §5.4).

**Repository:** `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership`
**Module:** Go 1.26.5+ · private module · deployed as a containerised microservice (HPA 2–8 replicas)
**Design:** Refines **IAM HLD v1.39 §5.6**; LLD v1.61 (Draft, 4799 lines). Where LLD and HLD disagree, HLD is authoritative.

---

## Mental model

This service is the authoritative store for the **business-layer organisation model** that sits between Keycloak's identity records and the workflow/tender domain.

| Layer | What it owns | What it does NOT own |
|-------|-------------|----------------------|
| **Tenants** | `plan`, subscription status, trial metadata, realm identity/type/shard, MFA freshness, `local_accounts_enabled`, licensed seats, ownerless/overage/realm-sync markers | JWT issuance, MFA enforcement — owned by Keycloak; `default_currency`, pricing — owned by Billing |
| **Plans catalogue** | Global operator-editable per-tier entitlements (`workflow_template_limit`, `tender_limit`, `sso_enabled`, `custom_branding`, `feature_set`, `trial_duration_days`) | Metered quota counters — owned by Usage & Metering (HLD §10.6) |
| **Departments** | Global operator catalog (`is_system`, `is_active`, never physically deleted) + per-tenant activation | Nothing internal |
| **Tenant memberships** | Lifecycle only, no role data (§16 A14) | Display identity, signature, OOO presentation flag — owned by User Profile |
| **Tenant-level role grants** | `tenant_owner`/`tenant_admin`/`tender_admin` only; `member` **derived at read time**, never persisted (TR-7, §16 A29) | Credentials, JWT — Keycloak |
| **Department memberships** | User↔department↔role-level (`preparator`/`reviewer`/`approver`) + tenant-customizable `dept_role_labels` | Tender content or bids — Tender Service |
| **Delegations** | Authoritative record for workflow rerouting (scope: all/department/tender; hard time bounds) | `user_availability` presentation — owned by User Profile |
| **Tender ACL overlays** | Additive `view`/`edit`/`approve` grants (§16 A32(c)) | `assignee_overrides` state — owned by Workflow Service (§16 A32(d), OVR-1) |
| **Pending invitations** | Two-step invite→accept staging; **one PII exception** — `email` + `full_name` until acceptance (§16 A38) | Notification delivery — Notification Service |

**Source of truth for one platform-wide contract:** `GET /api/v1/internal/users/:id/memberships` (I-8) — the hot-path membership projection consumed by AuthZ Enrichment on every authenticated request.

**Does NOT own:** credentials / MFA enforcement / JWT issuance (Keycloak); display identity / signature / OOO presentation flag (User Profile); realm or Keycloak Admin API mutations (Realm Provisioner — this service **never** calls the Keycloak Admin API); workflow template authoring or `assignee_overrides` state (Workflow Service); audit records (Audit Log); tender content / bids (Tender Service); metered quota consumption (Usage & Metering); pricing / currency / seat enforcement past grace (Billing); group→role/department mapping config (`group_dept_role_mappings`/`group_tenant_role_mappings`/`group_dept_mappings` and their admin CRUD — Group Mapping Service, ADR-0007 Wave 2; this service only *reads* the resolved result via `GroupMappingClient` for I-10 JIT resolution).

**Ownership split with User Profile — delegation:** `delegations` (here) is authoritative for workflow rerouting. `user_availability` (User Profile) is presentation-only. The coordination pattern (§8.6, CONS-2) validates the delegate → calls User Profile's `PUT /internal/users/:id/availability` → waits for 200 → **only then** inserts `delegations` + outbox event in one `RunInTx`. `DelegationStarted` is never enqueued without a committed User Profile update.

---

## Why this service exists

Keycloak owns authentication. It does not (and should not) model *how* a tenant's organisation is structured — plans, departments, elevated role grants, group→role mappings, delegations, ACL overlays, invitations. Without a dedicated service every downstream team would either duplicate that model or reach into Keycloak's Admin API, leading to:

- Divergent tenant metadata across services with no single source of truth
- Ad-hoc access-control logic reading from Keycloak groups directly, with no separation between identity and authorisation
- No transactional guarantee that a state change and its integration event land together
- No place to enforce SEAT-1 hard-cap, TM-8 last-owner protection, or the availability-first delegation ordering

This service **centralises** the tenant / membership / role model behind a single hot-path read endpoint (I-8) and two SNS event topics.

---

## API overview

All endpoints require `x-user-id` and `x-tenant-id` headers injected by the API gateway (or the mesh for internal calls).
Base path: `/api/v1` · Developer tools (non-production only): Swagger UI at `/swagger` · AsyncAPI viewer at `/asyncapi`

**51 endpoints** across three route prefixes with distinct auth models:

| Prefix | Callers | Auth | Ingress |
|---|---|---|---|
| `/api/v1/*` (31 endpoints, P-1..P-31) | Authenticated tenant users | Gateway-injected identity headers | Public Envoy |
| `/api/v1/internal/*` (13 endpoints, I-1..I-13) | In-mesh services (RP, Event Consumer, LLM, Signup BFF, Workflow, Billing, AuthZ) | Mesh mTLS + NetworkPolicy; **no JWT** | Internal Envoy only |
| `/api/v1/operator/*` (7 endpoints, O-1..O-7) | Human operators | Gateway validates JWT, asserts `platform_operator` claim | Separate operator ingress (§16 C1, AUTH-7) |

### Public routes (highlights)

| # | Method & Path | Purpose |
|---|---|---|
| P-1 | `GET /tenants/:id` | Tenant details (name, plan, locale, mfa_freshness) |
| P-2 | `PATCH /tenants/:id` | Update name/locale/local_accounts_enabled/mfa_freshness_seconds (drives RP `PatchRealmConfig`, T-15) |
| P-4 | `GET /tenants/:id/members` | List members (cursor-paginated) |
| P-6 | `POST /tenants/:id/members` | **Invite** — two-step invite→accept (§16 A11); SEAT-1 gated |
| P-7 | `PATCH /tenants/:id/members/:user_id` | Suspend / reactivate |
| P-8 | `DELETE /tenants/:id/members/:user_id` | Remove — **delegate-impact gated** (§8.8); `409 workflow_resolution_required` |
| P-10 | `PUT /tenants/:id/departments/:dept_id/members/:user_id` | Assign to dept at level; decrease is dept-scope delegate-impact gated (§8.8.4) |
| P-18/P-19/P-20 | `/delegations[/:id]` | List/create/cancel delegation (POST also coordinates User Profile) |
| P-21/P-22/P-23 | `/tenants/:id/tenders/:tender_id/acl` | Tender ACL grants |
| P-26 | `POST /tenants/:id/users/:user_id/removal-resolution` | Resolve blocked removal — `replace_delegate` or `stop_workflows` |
| P-27 | `GET /tenants/:id/seat-usage` | `{active_users, pending_invitations, licensed_seats, over_cap, overage_since, grace_ends_at}` |
| P-28 | `PUT /tenants/:id/members/:user_id/roles` | Full-replacement multi-role reconcile (TM-8 last-owner guard) |
| P-30/P-31 | `/tenants/:id/invitations[/:invitation_id]` | List / revoke pending invitations |

### Internal routes (highlights)

| # | Method & Path | Caller | Purpose |
|---|---|---|---|
| I-1 | `POST /tenants` | Realm Provisioner / Signup BFF | Provision tenant row |
| I-2 | `PATCH /tenants/:id` | Realm Provisioner | Set `realm_id`, `realm_type='dedicated'`, `keycloak_shard` together after provisioning |
| I-3 | `POST /tenants/:id/members` | Event Consumer | Add membership / **invitation-acceptance path** (§8.10 PI-4) |
| I-5 | `DELETE /tenants/:id/members/:user_id` | Event Consumer | Soft-delete on Keycloak `USER_DELETE` — delegate-impact gated (§8.8, WFI-1) |
| **I-8** | `GET /users/:id/memberships` | **AuthZ Enrichment (hot path)** | Full membership context; SLO 15 ms hit / 30 ms miss |
| I-10 | `POST /tenants/:id/dept-memberships` | Event Consumer | SAML group assertion → JIT dept memberships + additive tenant roles (GTRM-4) |
| I-11 | `GET /tenants/:id/seat-usage` | Billing | Pre-check before seat reduction |
| I-13 | `POST /tenants/:id/tenders/:tender_id/assignee-override` | Workflow Service | Validate-and-emit; O&M persists nothing (OVR-1) |

### Operator routes

O-1/O-2/O-3 (global-catalog departments) and O-5/O-6 (plan entitlement catalog) moved to
the **Catalog / Admin Config Service** per migration-runbook Phase 4 (ADR-0007) — this
service is no longer the writer (or reader) of record for `departments`/`plans`, both
tables have been dropped, and `OperatorService`/`OperatorHandler` now only implement O-4/O-7.

| # | Method & Path | Purpose |
|---|---|---|
| O-4 | `PATCH /tenants/:id/feature-flags` | Full-replacement of override delta (§16 A18, T-9) |
| O-7 | `POST /tenants/:id/reassign-owner` | Recover ownerless tenant (§16 A39, TM-12/T-13) |

Full endpoint catalogue with authZ, cache invalidation, and status-code table: [`.claude/api-caching-events.md § 5.3`](.claude/api-caching-events.md) and `api/openapi.yaml`.

---

## Architecture

Clean Architecture — dependencies point inward; outer layers never import inner layers.

```
iam-org-membership/
├── cmd/
│   ├── server/                       # Composition root: wiring, pool setup, middleware, exporter goroutines
│   └── reconciler/                   # Single-binary reconciler dispatched by --job=<name>; drives 8 CronJobs
├── internal/
│   ├── core/
│   │   ├── domain/                   # Entities, value objects, DomainError catalogue (no external deps)
│   │   ├── port/                     # Interfaces: repositories, cache, EventPublisher,
│   │   │                             #   UserProfileClient, WorkflowClient, RealmProvisionerClient
│   │   └── service/                  # Use cases + pure input validators
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                 # Gin handlers, DTOs, middleware
│       │   └── consumer/             # SQS consumer for tenant-orgm-q + billing-orgm-q
│       └── outbound/
│           ├── postgres/             # Repository impls + golang-migrate migrations
│           ├── valkey/               # Cache (go-redis/v9) — advisory only
│           ├── eventbus/             # RoutingPublisher (2 topics) + ValidatingCodec + outbox runner
│           │   └── schemas/*.json    # Embedded JSON Schema Draft-07 (source of truth for schema-gov)
│           ├── userprofile/          # HTTP client — SetAvailability (delegation coordination §8.6)
│           ├── workflow/             # HTTP client — GetDelegateImpact/Reassign/Cancel (§8.8)
│           ├── realmprovisioner/     # HTTP client — CreateInvitedUser/DeleteUser/PatchRealmConfig/RevokeUserSessions
│           └── metrics/              # Custom Prometheus counters (iam_*)
├── pkg/requestctx/                   # Typed RequestContext{UserID, TenantID, Roles, ClientIP, UserAgent}
├── api/
│   ├── openapi.yaml                  # OpenAPI 3.0 — REST contract (P-*/I-*/O-*)
│   └── asyncapi.yaml                 # AsyncAPI 3.0 — two channels
├── deploy/
│   ├── helm/                         # Helm chart (Deployment + 8 CronJobs)
│   ├── iam/                          # IRSA policies
│   └── monitoring/                   # Prometheus alert rules
├── scripts/                          # Local dev + build tooling
└── test/
    ├── unit/                         # Fast unit tests — no Docker required
    ├── postgres/                     # RLS + DB integration (testcontainers-go); Case 5 canonical
    ├── integration/                  # Cross-layer tests (LocalStack for SNS/SQS)
    ├── e2e/                          # End-to-end
    └── fixtures/                     # Shared fakes
```

Smoke tests (`make test-smoke`) drive the full-stack provisioning → member add → dept assign → delegation → expiry path against a staging deploy — not a Go test suite.

### Dependency rules (enforced by `go-arch-lint` in CI)

| Package | May import |
|---------|-----------|
| `core/domain` | Nothing outside itself |
| `core/port` | `core/domain` only |
| `core/service` | `core/domain` + `core/port` + `pkg/requestctx` |
| `adapter/*` | Implements `core/port`; nothing in `core/` imports `adapter/` |

### Storage and messaging

| Concern | Technology | Notes |
|---------|-----------|-------|
| **Primary store** | PostgreSQL 17 | 15 tables. RLS on all 12 tenant-scoped tables (`FORCE ROW LEVEL SECURITY`); GUC `app.tenant_id` set **transaction-locally** per checkout (RLS-6). `record_version` optimistic lock on 14 tables |
| **Connection pooling** | PgBouncer transaction mode | App connects via PgBouncer; migrations connect direct-to-Postgres via `MIGRATION_DATABASE_URL` (CONFIG-2) |
| **Cache** | Valkey (Redis-compatible) | Advisory-only (CACHE-2/9); 50 ms operation timeout = miss |
| **Events (outbound)** | AWS SNS + transactional outbox | Two topics: `iam.membership.events` (11 event types), `iam.tenant.events` (2 — `TenantCreated`, `TrialStarted`) |
| **Events (inbound)** | AWS SQS | Two queues: `tenant-orgm-q` (RP lifecycle), `billing-orgm-q` (plan/seat/status). DLQ `maxReceiveCount=5` |
| **Schema registry** | AWS Glue | Two registries (`iam-membership-events`, `iam-tenant-events`); governed by `platform-schemagov` |

### Shared library dependencies

| Library | Version | Purpose |
|---------|---------|---------|
| `platform-gincommon` | v1.2.0 | HTTP middleware, Zap logging, OTel tracing, Prometheus metrics, `RequestContext`, `PropagateHeaders` |
| `platform-pgcommon` | v1.1.1 | pgx/v5 pool, RLS GUC injection (`GUCSetFromContext`), `RunInTx`, migrations, error helpers |
| `platform-events` | v1.3.0 | Transactional outbox, `RoutingPublisher`, SQS consumer, CloudEvents envelope |
| `platform-schemagov` | v0.4 | Python 3.12 CLI (not a Go module); schema-registry governance via `docker run` in CI |

---

## Integrating with other services

This section is for **platform service authors** who need to read membership data, react to events, or call internal endpoints from another service in the mesh.

### 1. Prerequisites

Every caller must:

1. Run on the **internal service mesh** (mTLS + NetworkPolicy). Public internet cannot reach `/internal/*`.
2. Forward `x-user-id`, `x-tenant-id`, and `x-tenant-roles` headers — populated by your own middleware from the incoming request context.
3. Add `x-tenant-roles: iam-system` **only** on internal write calls. This role is reserved for the event consumer and other in-mesh services.

### 2. HTTP client setup

Use `platform-gincommon`'s `PropagateHeaders` to forward trace context and identity headers on every outbound call.

```go
import (
    "encoding/json"
    "fmt"
    "net/http"

    "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
    "github.com/gin-gonic/gin"
)

func callMemberships(c *gin.Context, baseURL, userID string) (*MembershipsResponse, error) {
    req, err := http.NewRequestWithContext(
        c.Request.Context(),
        http.MethodGet,
        fmt.Sprintf("%s/api/v1/internal/users/%s/memberships", baseURL, userID),
        nil,
    )
    if err != nil {
        return nil, err
    }
    // Injects: traceparent, x-request-id, x-user-id, x-tenant-id, x-tenant-roles
    gincommon.PropagateHeaders(c, req)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    var out MembershipsResponse
    return &out, json.NewDecoder(resp.Body).Decode(&out)
}
```

### 3. AuthZ Enrichment — hot path (I-8)

The single most important integration. Called on every authenticated request by AuthZ Enrichment. Returns a denormalised membership snapshot — no N+1 joins needed downstream.

```bash
GET /api/v1/internal/users/:id/memberships
x-user-id:       <system UUID>
x-tenant-id:     <tenant UUID>
x-tenant-roles:  iam-system
```

Response is a joined view over `tenant_memberships` + `tenants` + `tenant_roles` + `dept_memberships` + `delegations` (LLD §6.2), with:

- `departments[]` and `active_delegations[]` always arrays, never `null` (I8-4)
- Derived `member` role injected at projection layer (`resp.Roles = union(["member"], resp.Roles)` — TR-7 / §16 A29). The role is never stored in `tenant_roles`.
- `mfa_freshness_seconds` sourced from the tenant cache (`om:tenant`), evicted by P-2 writes — **not** from the frozen per-user snapshot (§16 A52, T-10)

Caching: the response is cached in Valkey under `om:memberships:{tenant}:{user}` with 300 s ± 30 s TTL (CACHE-4). Cache miss/timeout/outage always resolves from Postgres (CACHE-2, I8-2). Successful lookup is cached; `404` (no active membership) is **not** cached — AuthZ treats it as "no context → deny."

SLO: **15 ms p99 cache hit, 30 ms p99 cache miss.**

### 4. Delegation coordination — Org & Membership as the driver

When a user sets OOO with a delegate through O&M, O&M **drives the User Profile write** with strict ordering:

1. O&M validates delegate is an active same-tenant member (`422 invalid_delegate` on DEL-1 failure).
2. O&M calls User Profile: `PUT /internal/users/{delegator_id}/availability` with `{status: ooo, delegate_id, ooo_note, ends_at}`.
   - 4xx → `422 invalid_delegate` (delegate raced to inactive)
   - 5xx/timeout → `503 user_profile_unavailable` (retryable, no DB write)
   - 200 → continue
3. **Only if step 2 succeeds** does O&M commit `delegations` + emit `DelegationStarted` in one `RunInTx`.

This ordering guarantees no `DelegationStarted` event without a matching `user_availability` record (CONS-2, §8.6).

On **expiry** (`delegation-expiry` CronJob every 5 min), O&M calls UP's availability endpoint with `{delegate_id: null}` **only** — never `{status: available}` (§16 A49/J2 pointer-clear semantics). Ending a delegation is not the same as the delegator returning; the `ooo → available` transition is UP-owned (its `ooo_until` sweep or user's explicit "I'm back").

### 5. Event Consumer — user lifecycle

The IAM event consumer calls these endpoints on Keycloak webhook events:

| Keycloak event | O&M endpoint | Notes |
|----------------|--------------|-------|
| `REGISTER` (with matching pending invite) | `POST /internal/tenants/:id/members` (I-3) | Invitation-acceptance path (§8.10 PI-4) — flips pending row to `accepted`, applies queued roles/depts |
| `REGISTER` (no pending invite) | `POST /internal/tenants/:id/members` (I-3) | Direct add (JIT) |
| SAML group change | `POST /internal/tenants/:id/dept-memberships` (I-10) | JIT dept memberships + additive tenant roles (GTRM-4 — never revokes) |
| `USER_DELETE` | `DELETE /internal/tenants/:id/members/:user_id` (I-5) | **Delegate-impact gated** (§8.8, WFI-1); sets `ownerless_since` if last owner (TM-12) |
| Status change | `PATCH /internal/tenants/:id/members/:user_id` (I-4) | Update status from Keycloak lifecycle |

### 6. Workflow Service — delegate-impact (§8.8)

Every user removal and department demotion/removal calls Workflow **synchronously** to check for stranded workflows:

```
DELETE /tenants/:id/members/:user_id (P-8)
  → GET /internal/workflows/delegate-impact?tenant_id=&delegate_user_id= (query params)
    5xx/timeout → 503 workflow_service_unavailable (no DB write, WFI-8)
    active_workflows > 0 → 409 workflow_resolution_required
      body: {active_workflows, delegate_user_id, workflow_ids[],
             allowed_actions: [replace_delegate, stop_workflows]}
    active_workflows == 0 → proceed with removal cascade
```

The `409` is resolved via `POST /tenants/:id/users/:user_id/removal-resolution` (P-26):
- `action=replace_delegate` → `POST /internal/workflows/reassign-delegate` + re-check
- `action=stop_workflows` → `POST /internal/workflows/cancel-by-delegate` + re-check

Workflow → O&M direction is **empty** — no inbound subscription; all synchronous request/response (WFI-7).

### 7. Realm Provisioner — three call sites

O&M → RP:

- `POST /internal/tenants/:id/users` — Create invited user (§16 A11 invite flow). Fail-closed: 5xx returns `503 realm_provisioner_unavailable`.
- `DELETE /internal/tenants/:id/users/:keycloak_user_id` — Idempotent compensation (PI-9). Durable via `kc_cleanup_pending=true`; reconciler converges.
- `PATCH /internal/tenants/:id/realm-config` — Realm config propagation (T-15). Fail-open + durable: 5xx flips `realm_sync_pending=true` and returns 202; T-15 reconciler converges with disables prioritised as security-tightening.
- `POST /internal/tenants/:id/users/:keycloak_user_id/logout` — AUTH-8 session revocation on suspend/removal/deprivilege. Best-effort / fail-open; TTL backstop covers guaranteed cutoff.

RP → O&M: `POST /internal/tenants` (I-1) and `PATCH /internal/tenants/:id` (I-2, sets `realm_id`/`realm_type`/`keycloak_shard` together).

### 8. Subscribing to SNS events

O&M publishes to **two** SNS topics via a transactional outbox and `RoutingPublisher`. Subscribe via SQS filter policy on `event_type` MessageAttribute.

`tenant_id` is on the **envelope**, not the data. Read it from `envelope.tenant_id` — it is never duplicated inside `data`.

**`iam.membership.events`:**

| Event type | Data key fields | When to consume |
|---|---|---|
| `DepartmentMembershipGranted` | `user_id`, `department_id`, `level`, `actor_id` | AuthZ cache invalidation; RP approver role sync |
| `DepartmentMembershipRevoked` | `user_id`, `department_id`, `actor_id` | AuthZ cache invalidation |
| `DepartmentMembershipLevelChanged` | `user_id`, `department_id`, `previous_level`, `new_level` | AuthZ cache invalidation |
| `TenantRoleGranted` | `user_id`, `role_code`, `actor_id` (one event per role) | AuthZ cache; RP `requires-mfa` realm role for admin/owner |
| `TenantRoleRevoked` (§16 A14) | `user_id`, `role_code`, `actor_id` | AuthZ cache; symmetric with granted |
| `DelegationStarted` | `delegation_id`, `delegator_id`, `delegate_id`, `scope`, `scope_id`, `ends_at` | Workflow: reroute pending tickets |
| `DelegationEnded` | above + `ended_reason ∈ {expired, cancelled, delegate_removed}` (DEL-7) | Workflow: reroute back |
| `TenderAssigneeOverridden` | `tender_id`, `user_id`, `actor_id` | Workflow node reassignment (I-13 validate-and-emit) |
| `TenantSeatOverageStarted` | `tenant_id`, `licensed_seats`, `active_users`, `pending_invitations`, `overage_since` | Billing / CSM banner |
| `TenantSeatOverageResolved` | `tenant_id`, `resolved_at` | Billing / CSM banner |
| `TenantStateChanged` (§16 A61 relay) | `status`, `previous_status`, `plan`, `previous_plan`, `changed_at`, `cause` | Workflow (pause/resume/route without a direct tenant/billing subscription) |

**`iam.tenant.events`:** O&M produces only `TenantCreated` and `TrialStarted` on this topic. The other lifecycle events (`TrialTenantProvisioned`, `TenantRealmReady`, `TenantConverted`, `TrialExpired`, `TenantSuspended`, `TenantOffboarded`, …) are Realm-Provisioner-produced.

**Envelope shape:** `{id (UUID v7), source, tenant_id, trace_id, specversion, time, subject, actor, dataschema, data}`. `dataschema` is the Glue schema version UUID (absent under the dev `NoopCodec`).

**Idempotency:** Record `(id, consumer_name)` in your own `processed_events`-style table and skip duplicates — the outbox uses at-least-once delivery.

**Consumer contract — events are informational, not authorization grants.** Events describe what happened at the time they were published. They are delivered at-least-once, may arrive out of order, and reflect a point-in-time snapshot. **Do not make irreversible access-control decisions based solely on an event payload** — always re-read authoritative state from O&M (I-8) for decision-critical paths.

### 9. Handling errors

Standard `gincommon.ErrorResponse` shape carrying `{error, status, trace_id, request_id, code, message, details}` (LLD §17). `error` and `code` carry the same value.

Service-specific error codes worth special handling:

- **`workflow_resolution_required` (409, §8.8)** — body carries `active_workflows`, `workflow_ids[]`, `delegate_user_id`, `allowed_actions: [replace_delegate, stop_workflows]`. Present the two options to the admin; call P-26 with the chosen action.
- **`seat_limit_reached` (409, SEAT-1)** — body carries `licensed_seats`, `active_users`, `pending_invitations`. This is a **product signal, not an incident** — route to CSM/Billing (§20.6), not on-call.
- **`optimistic_lock_conflict` (409, CONC-4)** — response echoes current `record_version` and `updated_at`. Re-read and retry.
- **`user_profile_unavailable` (503)** — retryable; no DB write happened (§8.6).
- **`workflow_service_unavailable` (503)** — retryable; no DB write happened (WFI-8).
- **`realm_provisioner_unavailable` (503)** — retryable; no invitation created (§8.10).

### 10. Optimistic locking

All mutable responses include `record_version` (monotonic counter, DB-owned via `touch_row()` trigger — TRG-1). Pass it back as `expected_version` on updates to detect concurrent writes:

```go
tenant, _ := getTenant(id)  // tenant.RecordVersion == 3
patch := PatchTenantRequest{
    Name:            ptr("Acme Corp"),
    ExpectedVersion: ptr(tenant.RecordVersion),  // 3
}
// 409 optimistic_lock_conflict if another writer updated between GET and PATCH
```

CONC-4: 409 responses include the current `record_version` and `updated_at` so the caller can re-read and retry without a separate GET.

---

## Local development

### Prerequisites

- Go 1.26.5+ (must match `go.mod`)
- Docker + docker compose (required for PostgreSQL, PgBouncer, Valkey, LocalStack)
- `GOPRIVATE=github.com/BCBP-SOLUTIONS-FZC-LLC/*` (private modules); `GONOSUMDB` for the same prefix
- SSH key registered with the BCBP org for private module access

### Setup

```bash
make setup      # Copies .env-example → .env and installs .githooks/pre-commit
make tidy       # go mod tidy
make docker-up  # Start PostgreSQL + PgBouncer + Valkey + LocalStack (community)
make run        # Start the server on :8080
```

Host ports for the local stack are deliberately offset from the sibling `iam-user-profile2` service so both can run side-by-side:

| Container | Host port | Container port | Purpose |
|---|---|---|---|
| PgBouncer | `5533` | `5432` | Connection pooler (transaction mode) — app connects here |
| Postgres (direct) | `5534` | `5432` | Direct connection for migrations |
| Valkey | `6380` | `6379` | Cache |
| LocalStack | `4567` | `4566` | S3 / SNS / SQS / Glue (Pro) |

Sibling `iam-user-profile2` holds `5433 / 5434 / 6379 / 4566`.

For LocalStack Pro (Glue Schema Registry + KMS), set `LOCALSTACK_AUTH_TOKEN` in `.env` and run `make docker-up-pro` instead.

### Common commands

Run `make help` for the full list. Highlights:

| Command | Description |
|---------|-------------|
| `make setup` | Copy `.env-example` → `.env` and install `.githooks/pre-commit` |
| `make tidy` | `go mod tidy` |
| `make fmt` / `make fmt-check` | Format / verify formatting (mirrors CI) |
| `make vet` / `make lint` | Static analysis |
| `make mod-verify` | `go mod verify` — check module download integrity |
| `make vuln-check` | `govulncheck ./internal/...` |
| `make test` | All tests (unit + postgres + integration + e2e; requires Docker) |
| `make test-ci` | Unit + postgres + integration with `-race` + coverage (used in CI) |
| `make test-unit` | Unit tests only — no Docker |
| `make test-postgres` | Postgres + RLS integration via testcontainers-go |
| `make test-integration` | Cross-layer integration tests (SNS/SQS via LocalStack) |
| `make test-e2e` | End-to-end tests |
| `make test-smoke` | Staging smoke path: provision → member add → dept assign → delegation → expiry |
| `make race` | All tests with `-race` |
| `make cover` / `make cover-func` | Coverage HTML report / per-function summary |
| `make ci` | `tidy + fmt-check + vet + lint + test-ci + build` (full CI pipeline) |
| `make run` | Kill port 8080 and `go run ./cmd/server` |
| `make build` | Compile `bin/iam-org-membership` + `bin/reconciler` |
| `make docker-up` | Start PostgreSQL + PgBouncer + Valkey + LocalStack (community) |
| `make docker-up-pro` | Same with LocalStack Pro (requires `LOCALSTACK_AUTH_TOKEN`) |
| `make docker-down` | Stop containers |
| `make clean` | Remove `bin/` artefacts and coverage files |
| `make extract-schemas` | Derive `internal/adapter/outbound/eventbus/schemas/*.json` from `api/asyncapi.yaml` (Docker) |
| `make schema-validate` | Validate AsyncAPI + event schemas via `schema-gov validate` |
| `make schema-diff CURRENT=… PROPOSED=…` | Show compatibility diff between two schema files |
| `make schema-register` | Register event schemas to Glue (requires `GLUE_REGISTRY_NAME_*`) |
| `make schema-verify` | Pre-deploy check that every expected PascalCase Glue schema exists |
| `make schema-prune` | Dry-run: list orphaned Glue schemas |

### Running a single test

```bash
go test ./test/unit/membership_service/... -run TestRemoveUserDelegateImpact -v
go test ./test/postgres/... -run TestRLSPolicyEnforcement -v
go test ./test/integration/... -run TestEVT14RecencyGuard -v
```

### Calling the API locally

```bash
# After make docker-up and make run:

# I-8 hot path (system principal, target tenant context)
curl -i http://localhost:8080/api/v1/internal/users/a1b2c3d4-e5f6-7890-abcd-ef1234567890/memberships \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: f0e1d2c3-b4a5-6789-0fed-cba987654321" \
  -H "x-tenant-roles: iam-system"

# Tenant details (public)
curl -i http://localhost:8080/api/v1/tenants/f0e1d2c3-b4a5-6789-0fed-cba987654321 \
  -H "x-user-id: a1b2c3d4-e5f6-7890-abcd-ef1234567890" \
  -H "x-tenant-id: f0e1d2c3-b4a5-6789-0fed-cba987654321" \
  -H "x-tenant-roles: tenant_owner"

# Invite (P-6) — SEAT-1 gated, returns 202 on stage
curl -i -X POST http://localhost:8080/api/v1/tenants/f0e1d2c3-b4a5-6789-0fed-cba987654321/members \
  -H "Content-Type: application/json" \
  -H "x-user-id: a1b2c3d4-e5f6-7890-abcd-ef1234567890" \
  -H "x-tenant-id: f0e1d2c3-b4a5-6789-0fed-cba987654321" \
  -H "x-tenant-roles: tenant_admin" \
  -d '{"email":"newuser@acmecorp.com","full_name":"New User","initial_tenant_roles":[],"initial_dept_mappings":[{"department_id":"...","role_level":"preparator"}]}'
```

---

## Testing domain events locally

Every write publishes a domain event through a **transactional outbox → SNS → SQS** pipeline. This section shows how to observe that pipeline end-to-end on your laptop.

### How the pipeline works

```
HTTP write
    │
    ▼
service layer  ──(same tx)──▶  outbox_events (Postgres)
                                      │
                               outbox runner (500 ms poll)
                                      │
                                      ▼
                               RoutingPublisher                            (chooses topic by Envelope.Source)
                               ├── iam.membership.events  →  SNS  (LocalStack)
                               └── iam.tenant.events      →  SNS  (LocalStack)
                                                                  │
                                                          SNS fan-out to subscribing SQS queues
                                                                  │
                                                          your consumer / awslocal / LocalStack Desktop
```

The outbox insert is atomic with the business write (EVT-10, CONS-1) — if the HTTP request succeeds (2xx) an event **will** appear in `outbox_events`. The runner delivers it within one poll interval (default 500 ms).

### Step 1 — Start infrastructure

```bash
make docker-up
```

`docker-compose.yml` starts PostgreSQL (direct on 5534), PgBouncer (5533), Valkey (6380), and LocalStack (4567). The LocalStack init script (`scripts/init-localstack.sh`) provisions the full §7.1 / §7.3.2 SNS/SQS topology on first start:

| Resource | Type | Notes |
|---|---|---|
| `iam-membership-events` | SNS topic | 11 event types |
| `iam-tenant-events` | SNS topic | O&M produces only `TenantCreated` / `TrialStarted` |
| `tenant-orgm-q` | SQS queue | Subscribed to `iam-tenant-events` (RP-produced lifecycle) |
| `billing-orgm-q` | SQS queue | Subscribed to `billing.events` (plan/seat/status) |
| Fan-out queues | SQS | `membership-audit-q`, `membership-authz-q`, `membership-workflow-q`, etc. |

All queues have a `-dlq` sibling with `maxReceiveCount=5`.

### Step 2 — Verify SNS/SQS exist

```bash
docker compose exec localstack awslocal sns list-topics --region ap-south-1
docker compose exec localstack awslocal sqs list-queues --region ap-south-1

# Check queue depth
docker compose exec localstack awslocal sqs get-queue-attributes \
  --queue-url http://localhost:4566/000000000000/tenant-orgm-q \
  --attribute-names ApproximateNumberOfMessages \
  --region ap-south-1
```

### Step 3 — Trigger an event

Provision a tenant via the internal route (system principal + target tenant context):

```bash
TENANT_ID=$(python3 -c 'import uuid; print(uuid.uuid4())')

curl -s -X POST http://localhost:8080/api/v1/internal/tenants \
  -H "Content-Type: application/json" \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: $TENANT_ID" \
  -H "x-tenant-roles: iam-system" \
  -d "{\"id\":\"$TENANT_ID\",\"slug\":\"acme-$RANDOM\",\"plan\":\"starter\",\"name\":\"Acme\",\"owner_user_id\":\"a1b2c3d4-e5f6-7890-abcd-ef1234567890\"}" | jq .
```

A `201` response means the tenant row, 5 system-department activations (Engineering, Design, Procurement, Finance, Legal), 3 `dept_role_labels`, the owner membership, the owner `tenant_owner` grant, and both outbox events (`TenantCreated` + `TrialStarted`) were committed atomically in one `RunInTx` (§8.1). Expect both events on `iam-tenant-events` within ~500 ms.

### Step 4 — Read messages from SQS

```bash
docker compose exec localstack awslocal sqs receive-message \
  --queue-url http://localhost:4566/000000000000/tenant-orgm-q \
  --max-number-of-messages 10 \
  --region ap-south-1 | jq '.Messages[].Body | fromjson'
```

Example `TenantCreated` envelope:

```json
{
  "id":          "019ee815-610d-73d8-87fa-ad523fe75a33",
  "type":        "TenantCreated",
  "source":      "iam.tenant.events",
  "tenant_id":   "f0e1d2c3-b4a5-6789-0fed-cba987654321",
  "subject":     "f0e1d2c3-b4a5-6789-0fed-cba987654321",
  "actor":       "00000000-0000-0000-0000-0000000000a1",
  "trace_id":    "425f7b61f1af064f3b84e42503a3e4d1",
  "time":        "2026-07-22T02:49:35.757252Z",
  "specversion": "1",
  "data": {
    "tenant_id": "f0e1d2c3-b4a5-6789-0fed-cba987654321",
    "slug":      "acme-4271",
    "plan":      "starter",
    "status":    "trial"
  }
}
```

> **`tenant_id` is in the envelope, not the payload.** Read it from the top-level `tenant_id` field — it is intentionally absent from `data`.

### Step 5 — Inspect the outbox

```bash
docker compose exec postgres psql -U org_membership_app -d org_membership -c \
  "SELECT id, event_type, attempts, published_at IS NOT NULL AS published, last_error
   FROM outbox_events ORDER BY created_at DESC LIMIT 20;"
```

`published_at IS NOT NULL` means the runner delivered to SNS; the row will be pruned by the `outbox-prune` CronJob.

### Troubleshooting events

| Symptom | Likely cause | Fix |
|---|---|---|
| SQS queue empty after a successful write | SNS topic / subscription not created | Re-run `scripts/init-localstack.sh` or `make docker-down && make docker-up` |
| `outbox_events` row has `last_error` `404 NotFound` | Topic ARN mismatch | Verify `SNS_TOPIC_MEMBERSHIP_ARN` / `SNS_TOPIC_TENANT_ARN` in `.env` |
| `outbox_events` empty after a write | Row published + pruned, **or** the write failed | Re-check the HTTP response code |
| Messages reappear after `receive-message` | Normal — visibility timeout. Use `delete-message` to remove permanently |
| `GLUE_REGISTRY_NAME` set + inserts fail with `invalid input syntax for type json` | Glue-encoded bytes are not valid JSONB. **Always leave `GLUE_REGISTRY_NAME_*` empty in local dev.** Under `PG_BOUNCER_MODE=true` the 18-byte Glue header corrupts `outbox_events.payload`. |

---

## Testing

- Tests live under **`test/`** (separate package tree from `internal/`).
- **Testcontainers:** PostgreSQL, Valkey, and LocalStack integration tests spin up real containers. Docker must be running. Pass `-short` to skip them without Docker.
- **`test/fixtures/`** provides shared fakes for all three outbound HTTP clients (`FakeWorkflowClient`, `FakeUserProfileClient`, `FakeRealmProvisionerClient`), plus DB helpers and event assertions.

### Canonical tests (do not break)

| Test | Invariant |
|---|---|
| `test/postgres/rls_test.go::TestCase5_NoCrossTenantLeakAcrossPooledBackend` | RLS-6 — transaction-local GUC binding under PgBouncer |
| `test/unit/membership_service/TestRemoveUserDelegateImpact` | §8.8 pre-check + 409 body shape (WFI-3) |
| `test/postgres/TestOptimisticLockConflict` | CONC-3/4 — 409 echoes current `record_version` + `updated_at` |
| `test/integration/TestEVT14RecencyGuard` | Stale event silently skipped, still records `processed_events` |
| `test/integration/TestEVT15FutureTimeClamp` | Poison-pill DLQ, not recorded in `processed_events` |
| `test/integration/TestEVT16TenantStateRelay` | `TenantStateChanged` emitted in same tx as projection UPDATE |
| `test/integration/TestSeatCap_Concurrent` | Concurrent P-6 with 1 remaining seat → exactly one succeeds (row-lock) |
| `test/integration/TestTM12LastOwnerEscalation` | I-5 sets `ownerless_since`; only O-7 clears |

### Coverage

Coverage is measured over `./internal/...` and `./pkg/...` only. Aim for meaningful coverage on validators, RLS-guarded repositories, publisher paths, delegate-impact resolution branches, and the EVT-14/15/16 handlers rather than chasing 100 % on trivial code.

```bash
make cover-func   # per-function summary in terminal
make cover        # HTML report
```

---

## Environment variables

The service will not start without the variables marked **required** (`cmd/server/main.go::validateRequiredEnv`).

| Variable | Example | Default | Purpose |
|----------|---------|---------|---------|
| `APP_NAME` | `iam-org-membership` | `iam-org-membership` | Prometheus label, OTel service name |
| `APP_ENV` | `dev` / `prod` | `dev` | Zap log format; controls whether dev tools (`/swagger`, `/asyncapi`) are served |
| `APP_PORT` | `8080` | `8080` | HTTP listen port |
| `DATABASE_URL` | `postgres://...` | **required** | Full DSN (overrides individual `PG_*` vars); pool connects here via PgBouncer |
| `MIGRATION_DATABASE_URL` | `postgres://...@localhost:5534/...` | **required** | **Direct Postgres DSN for migrations** (bypasses PgBouncer — advisory locks are session-scoped, DDL incompatible with transaction pooling, CONFIG-2/MIG-3). In local dev use host port 5534; in deployed environments use the in-cluster Postgres service on 5432. **Must NOT point at PgBouncer.** |
| `SYSTEM_DATABASE_URL` | `postgres://org_membership_migrator:...@postgres:5432/...` | *(fallback to `DATABASE_URL` in dev with warning)* | **BYPASSRLS pool** for reconciler jobs + cross-tenant metric exporters (RLS-4). In prod must target `org_membership_migrator`. In dev falls back to `DATABASE_URL` and cross-tenant queries are RLS-filtered to 0 rows |
| `PG_BOUNCER_MODE` | `true` / `false` | `false` (prod default `true`) | Enables simple-query protocol; required when the pool routes through PgBouncer transaction pooling |
| `PG_MAX_CONNS` | `15` | `20` | Pool max connections per pod |
| `PG_MIN_CONNS` | `0` | `0` | Pool min idle connections |
| `PG_SLOW_QUERY_THRESHOLD` | `200ms` | `200ms` | Log queries slower than this (WARN, `tenant_id` redacted) |
| `VALKEY_URL` | `rediss://valkey:6379` / `localhost:6380` | **required** | ElastiCache / Valkey endpoint. **Must use `rediss://` in production/staging** |
| `VALKEY_TIMEOUT_MS` | `50` | `50` | Cache operation timeout (miss on timeout, CONFIG-3) |
| `SNS_TOPIC_MEMBERSHIP_ARN` | `arn:aws:sns:...:iam-membership-events` | **required** | `iam.membership.events` topic ARN |
| `SNS_TOPIC_TENANT_ARN` | `arn:aws:sns:...:iam-tenant-events` | **required** | `iam.tenant.events` topic ARN |
| `SQS_TENANT_ORGM_QUEUE_URL` | `https://sqs...tenant-orgm-q` | **required** | Inbound queue for RP lifecycle events |
| `SQS_BILLING_ORGM_QUEUE_URL` | `https://sqs...billing-orgm-q` | **required** | Inbound queue for Billing events |
| `AWS_REGION` | `ap-south-1` | `ap-south-1` | AWS region |
| `AWS_ACCESS_KEY_ID` | `localstack` | — | AWS credentials (use IRSA in production) |
| `AWS_SECRET_ACCESS_KEY` | `localstack` | — | AWS credentials |
| `AWS_ENDPOINT_URL` | `http://localhost:4567` | — | LocalStack endpoint (leave unset in production) |
| `GLUE_REGISTRY_NAME_MEMBERSHIP` | `iam-membership-events` | — | Glue registry for membership events (omit in dev — `NoopCodec` fallback) |
| `GLUE_REGISTRY_NAME_TENANT` | `iam-tenant-events` | — | Glue registry for tenant events |
| `USER_PROFILE_SERVICE_BASE_URL` | `http://user-profile:8080` | **required** | User Profile base URL (delegation coordination §8.6) |
| `USER_PROFILE_TIMEOUT_MS` | `3000` | `3000` | User Profile HTTP timeout |
| `WORKFLOW_SERVICE_BASE_URL` | `http://workflow:8080` | **required** | Workflow Service base URL (§8.8 delegate-impact) |
| `WORKFLOW_TIMEOUT_MS` | `3000` | `3000` | Workflow HTTP timeout |
| `REALM_PROVISIONER_BASE_URL` | `http://realm-provisioner:8080` | **required** | Realm Provisioner base URL (invite create/delete, realm config, session revoke) |
| `REALM_PROVISIONER_TIMEOUT_MS` | `3000` | `3000` | Realm Provisioner HTTP timeout |
| `INVITATION_EXPIRY_DAYS` | `7` | `7` | Pending invitation window. **Must equal Keycloak invite action-token lifespan** — divergence strands seats or frees them while the link still works |
| `INVITE_REINVITE_COOLDOWN_MINUTES` | `15` | `15` | Per-email cooldown (PI-11); `0` disables |
| `INVITE_MAX_PER_TENANT_PER_HOUR` | `60` | `60` | Per-tenant hourly ceiling (PI-12); `0` disables |
| `SEAT_OVERAGE_GRACE_DAYS` | `30` | `30` | Drives `grace_ends_at` + past-grace alert. **Not** an auto-action trigger (SEAT-3/SEAT-4) — Billing's enforcement decision |
| `MAX_LIFECYCLE_EVENT_SKEW_SECONDS` | `300` | `300` | EVT-15 future-time clamp threshold — events beyond this go to DLQ, not recorded in `processed_events` |
| `OUTBOX_POLL_INTERVAL` | `500ms` | `500ms` | Outbox runner poll frequency |
| `OUTBOX_BATCH_SIZE` | `50` | `50` | Events per outbox batch |
| `OUTBOX_MAX_ATTEMPTS` | `5` | `5` | Max retry attempts before dead-letter (EVT-5) |
| `OUTBOX_DRAIN_TIMEOUT` | `30s` | `30s` | Graceful-shutdown drain window |
| `OUTBOX_PUBLISH_CONCURRENCY` | `4` | `4` | Concurrent SNS publish goroutines |
| `OUTBOX_STARTUP_JITTER` | `2s` | `2s` | Random delay before first outbox poll (HPA thundering-herd) |
| `OUTBOX_CLAIM_LEASE_DURATION` | `10m` | `10m` | Lease duration before another runner may re-claim a batch |
| `PROCESSED_EVENTS_TTL_DAYS` | `8` | `8` | `processed_events` retention (PE-1: strictly > 7-day SQS lifetime; IDEMP-4 backstop window) |
| `CACHE_TTL_SECONDS` | `300` | `300` | Base TTL for `om:*` cache keys (actual TTL = base ± jitter) |
| `OTEL_SERVICE_NAME` | `iam-org-membership` | `APP_NAME` | OTel `service.name` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `otel-collector:4317` | — | OTLP/gRPC collector |
| `OTEL_EXPORTER_OTLP_INSECURE` | `true` / `false` | `true` (dev) | Plaintext OTLP |
| `DOCS_ENABLED` | `true` / `false` | `false` | Opt-in to serving `/swagger` and `/asyncapi` in production (requires `DOCS_AUTH_TOKEN`) |
| `DOCS_AUTH_TOKEN` | `<random-secret>` | — | Bearer token for `/swagger` and `/asyncapi` in production |
| `BUILD_VERSION` | `v1.0.0-abc123` | — | CI-injected build tag |

---

## Security

| Topic | Guidance |
|---|---|
| **Trust boundary** | `x-user-id`, `x-tenant-id`, `x-tenant-roles` must be injected by a **trusted API gateway** after authentication. Never allow clients to set these headers directly. The gateway strips client-supplied `x-tenant-roles` and sources `platform_operator` claim only from the operator IdP (AUTH-7, gateway/platform-security team contract) |
| **Row-Level Security** | Every PostgreSQL query runs with `app.tenant_id` GUC set **transaction-locally**. RLS `FORCE` policies + `REVOKE ALL FROM PUBLIC` on 12 tenant-scoped tables. Missing / mismatched GUC fails closed (RLS-1..6). `tenants` policy uses `id = current_setting('app.tenant_id')::uuid` (single-row visibility). Cross-tenant admin needs the `BYPASSRLS` role `org_membership_migrator`, never the app role `org_membership_app` (CI-verified via MIG-3/MIG-5) |
| **Operator route defense-in-depth (AUTH-7)** | Three layers: (1) NetworkPolicy blocks public network from reaching operator ingress; (2) handler re-checks `platform_operator` from `rc.Roles` before any DB access (AUTH-6); (3) gateway strips client-supplied `x-tenant-roles` and sources `platform_operator` claim from operator IdP only |
| **Session revocation (AUTH-8)** | Privilege reduction (P-7 suspend, P-8 removal, P-28 de-privilege) commits O&M state first, then makes a **best-effort, fail-open** `RevokeUserSessions` call to Realm Provisioner. Guaranteed cutoff falls back to TTL backstop (≤ access-token lifetime + 300 s `om:memberships` cache). Sustained `iam_session_revoke_failed_total` rate pages |
| **Input validation** | `slug`: `^[a-z0-9][a-z0-9-]{2,62}[a-z0-9]$`, immutable after set (T-1, `trg_tenant_slug_immutable`). `default_locale`: BCP-47. `keycloak_group_name`: ≤200 chars, `^[a-zA-Z0-9_./-]+$`. All UUIDs validated at handler layer. `mfa_freshness_seconds ∈ [60, 900]` (T-10) |
| **PII posture** | O&M stores **no PII beyond opaque UUIDs**, except `pending_invitations` (§16 A38 — `email` + `full_name` until acceptance; the invitee has no Keycloak/UP identity yet). GDPR erasure for pending invitations is the sole path keyed on email (citext), not `user_id` |
| **`member` role never persisted** | `chk_tr_no_member` bars `member` on `tenant_roles` (`chk_gtrm_no_member` enforced the same rule on `group_tenant_role_mappings` before that table moved to Group Mapping Service, ADR-0007 Wave 2). The role is injected at read time by I-8 at the projection layer (TR-7 / §16 A29) |
| **No Keycloak Admin API calls** | O&M never calls the Keycloak Admin API. All KC mutations go via Realm Provisioner (HLD §5.2). CI reviews check for `keycloak/*` imports outside the `realmprovisioner` HTTP client |
| **`SET app.tenant_id` forbidden pattern** | CI greps for non-`LOCAL` `SET app.tenant_id`. A session-scoped `SET` would persist on a pooled PgBouncer backend and leak the last tenant's GUC to the next request (RLS-6 violation) |
| **Developer tools auth** | When `APP_ENV=production`, `/swagger` and `/asyncapi` routes require `Authorization: Bearer <DOCS_AUTH_TOKEN>`. Set `DOCS_ENABLED=false` to eliminate the routes entirely |

---

## Observability

### SLOs (§11.1)

| Endpoint | p99 |
|---|---|
| I-8 cache hit | 15 ms |
| I-8 cache miss | 30 ms |
| P-6 invite (incl. SEAT-1 `FOR UPDATE` + RP call) | 100 ms |
| P-19 delegation create (incl. UP call) | 200 ms |
| P-8 removal (incl. `GetDelegateImpact`) | 200 ms |
| P-26 removal-resolution (reassign/cancel + re-check) | 350 ms |
| Outbound event publish half (outbox commit → SNS) | 1 s |
| `DelegationStarted` end-to-end (→ Workflow reroute) | 5 s joint (O&M owns publish half only) |
| Inbound lifecycle projection freshness (producer publish → `tenants` reflects) | 30 s |

**SLO-3** is the primary drift signal — EVT-14 skips stale events **silently**, so the lag gauge is the drift signal, not DLQ depth.

### Metrics (`iam_*` prefix)

All Prometheus metric names carry a `job` label to disambiguate emitting service (§16 A50/J4). Cardinality-bounded — `tenant_id` capped at tenant count (~1500–2000 target); no unbounded / user-supplied labels (§16 A48).

Load-bearing metrics:

- `iam_membership_lookup_latency_seconds{result=hit|miss}` — I-8 latency, source for SLO-1
- `iam_memberships_cache_hit_ratio` — HPA input signal
- `iam_delegate_removal_blocked_total{trigger=full_removal|dept_demotion|dept_removal}` — 409 `workflow_resolution_required` rate
- `iam_seat_limit_reached_total` — P-6 refused; **product signal, not an incident**
- `iam_stale_lifecycle_event_skipped_total` — EVT-14 stale skip (post-DLQ-redrive spikes expected)
- `iam_future_lifecycle_event_rejected_total` — EVT-15 clamp; **any nonzero pages** (producer clock skew)
- `iam_tenant_ownerless` — `count(*) WHERE ownerless_since IS NOT NULL`; **any nonzero pages `platform_operator`** (O-7 required)
- `iam_realm_sync_pending` — `count(*) WHERE realm_sync_pending`; sustained beyond ~10 min pages (T-15)
- `iam_session_revoke_failed_total` — sustained rate pages (AUTH-8 fast-kill degraded)
- `iam_lifecycle_consumer_lag_seconds` — SLO-3 primary drift signal
- `iam_delegation_expiry_deferred_total` — sustained rate warns (UP degraded)

Full catalogue and alert routing: [`.claude/operations.md § 11.2`](.claude/operations.md).

### Tracing and logs

`platform-gincommon.InitTracingFromEnv()` + `platform-pgcommon.NewOTelQueryTracer`. W3C `traceparent` propagated via `gincommon.PropagateHeaders` on every outbound call (User Profile, Workflow, Realm Provisioner). Delegation flow has parent span `delegation.create` with child spans for UP HTTP + DB write.

Zap structured logs: slow queries > 200 ms at WARN (`tenant_id` redacted); RLS violations at ERROR (1% sampled); delegation lifecycle at INFO; `tenant_ownerless_escalation` at ERROR (durable, page-worthy from I-5).

---

## Deployment

The Helm chart in `deploy/helm/` renders a single `Deployment` (server) plus 8 `CronJob` resources, all sharing the same image with a `command: ["/reconciler", "--job=<name>"]` override:

| CronJob | Schedule | Purpose |
|---|---|---|
| `delegation-expiry` | `*/5 * * * *` | Expire past `ends_at`; defers on UP 5xx (DEL-6) |
| `invitation-expiry` | `*/15 * * * *` | Past-`expires_at` pending → `expired` + `kc_cleanup_pending=true` (PI-5) |
| `invitation-kc-cleanup` | `*/10 * * * *` | Saga-compensation (PI-9): sweep `kc_cleanup_pending`, call `RP.DeleteUser`, clear flag |
| `realm-config-sync` | `*/2 * * * *` | T-15 reconciler; disables prioritised (security-tightening) |
| `seat-overage-reconcile` | `0 * * * *` | Seat-overage marker backstop (SEAT-5); drives past-grace alert |
| `trial-cleanup` | `0 2 * * *` | Phase-2 DB executor: soft-delete + PII-scrub for `trial_expired` past 15-d grace (§15.3) |
| `outbox-prune` | `0 1 * * *` | `outbox.Runner.PrunePublished(24h, 10000)` |
| `processed-events-prune` | `0 * * * *` | Delete `processed_events > 8 days` (PE-1 / IDEMP-4) |

**HPA:** 2–8 replicas on CPU + `iam_memberships_cache_hit_ratio`.
**PDB:** `minAvailable: 1`.
**Resources:** CPU 100m/500m, Memory 128Mi/384Mi.
**terminationGracePeriodSeconds:** `75` (drains the outbox and in-flight consumer messages).

The 4 metric exporters (`iam_tenant_ownerless`, `iam_realm_sync_pending`, `iam_seat_overage_tenants`, `iam_pending_invitations`) run as **ticker goroutines inside `cmd/server`**, not as CronJobs — matches the sibling `iam-user-profile2` pattern.

### CI

GitHub Actions runs on push/PR to `main`:

- **`validate-test`** — Race detector + coverage gate on merged `coverage.out` (unit + postgres + integration).
- **`validate-quality`** — `gofmt`, `go mod tidy`, `go vet`, `golangci-lint`, `govulncheck`, `go mod verify`.
- **`arch-lint`** — `go-arch-lint` enforces Clean Architecture dependency rules.
- **`schema-registry`** — `schema-gov extract --check` + `validate` + `enforce-lifecycle` + `diff` (PR); `register` + `changelog` + `metrics` on `main`.
- **`build-image`** — Docker Buildx (`linux/amd64`, distroless static-debian12:nonroot) with GHA + GHCR registry cache; Hadolint + `.dockerignore` lint before build.
- **`trivy`** — CVE scan (CRITICAL/HIGH/UNKNOWN); SARIF uploaded to Security tab; CycloneDX SBOM retained 90 days.
- **`smoke`** — Smoke tests against the cached image.
- **`changelog-check`** — Fails PRs that touch `internal/`, `api/`, `deploy/`, or `cmd/` without a `CHANGELOG.md` entry.

**Push to `main`** — Builds and pushes to GHCR, signs with Cosign keyless signing (Sigstore OIDC), self-verifies.

**Release (`v*` tags)** — validate → build → docker (CVE scan + SBOM + Cosign sign+verify) → **deploy-gate** (Helm upgrade + rollout verify + 2-min error-rate check; auto-rollback on failure) → GitHub Release with semver tags, `checksums.txt`, CycloneDX SBOM, and SLSA provenance.

---

## Docker

### The published image ships two binaries

| Binary | Path in image | Purpose |
|---|---|---|
| `iam-org-membership` | `/iam-org-membership` | HTTP server (`cmd/server`) — long-running |
| `reconciler` | `/reconciler` | Single-binary reconciler (`cmd/reconciler`) — one-shot, dispatched by `--job=<name>` |

Two-stage Dockerfile: Go builder → `gcr.io/distroless/static-debian12:nonroot`. Final image under 20 MB, runs as non-root, no shell.

### What the bundled `docker-compose.yml` starts

`docker-compose.yml` starts the **infrastructure dependencies only** — not the service itself. The service runs via `make run` for fast rebuilds.

| Container | Image | Host port | Purpose |
|---|---|---|---|
| `postgres` | `postgres:17-alpine` | `5534` | Primary store (direct connection for migrations) |
| `pgbouncer` | `edoburu/pgbouncer:latest` | `5533` | Connection pooler (transaction mode) — app connects here |
| `valkey` | `valkey/valkey:8-alpine` | `6380` | Cache |
| `localstack` | `localstack/localstack:4.4.0` | `4567` | S3, SNS, SQS (community); Glue + KMS require Pro |

> **LocalStack community vs Pro:** the default `docker-compose.yml` uses the community image (S3, SNS, SQS). Glue Schema Registry and KMS are Pro-only. `GLUE_REGISTRY_NAME_*` must be **unset** in local dev — the service falls back to `NoopCodec` (plain JSON, no Glue wire-format header). Setting `GLUE_REGISTRY_NAME_*` under `PG_BOUNCER_MODE=true` will corrupt `outbox_events.payload` because the 18-byte Glue header is not valid JSONB. Use `make docker-up-pro` and set `LOCALSTACK_AUTH_TOKEN` in `.env` for Glue.

### Building the service image

```bash
docker build -t iam-org-membership:local .

docker run --rm --network host --env-file .env \
  -e PG_HOST=localhost -e VALKEY_URL=localhost:6380 \
  -e AWS_ENDPOINT_URL=http://localhost:4567 \
  iam-org-membership:local
```

### Health and readiness

| Endpoint | Returns | Use for |
|---|---|---|
| `GET /healthz` | `200 {"status":"ok"}` | Liveness probe — no auth required |
| `GET /readyz` | `200 {"status":"ready"}` or `503` | Readiness probe — checks DB + cache connectivity |

`/readyz` reports **degraded but still ready** on cache failure alone (Postgres healthy) — CACHE-9.

---

## Cross-service dependencies

For write operations, O&M availability = O&M × dependency (except fail-open). **Reads (I-8 hot path, list endpoints) have NO synchronous cross-service dependency** — Postgres + Valkey only — so authN/authZ stays available even when every write dependency is down (§20.7).

| Operation | Sync dependency | Posture | On failure |
|---|---|---|---|
| Invite (P-6) | RP `CreateInvitedUser` | **fail-closed** | `503 realm_provisioner_unavailable`, no invitation, retryable |
| Delegation create (P-19) | UP `SetAvailability` | **fail-closed** | `503 user_profile_unavailable`, no delegation, retryable |
| User removal / dept demotion·removal (P-8/I-5/P-10/P-11) | Workflow `GetDelegateImpact` | **fail-closed** | `503 workflow_service_unavailable`, no change (WFI-8) |
| Removal resolution (P-26) | Workflow reassign/cancel + re-check | **fail-closed** | `503`, no DB write |
| Suspension (P-7) | Workflow `GetDelegateImpact` (advisory) | **fail-open** | suspend commits, advisory omitted |
| `local_accounts_enabled` change (P-2) | RP `PatchRealmConfig` | **fail-open + durable reconcile** | commits, `realm_sync_pending=true`, 202 |
| Invite compensation / revoke / expiry KC-cleanup | RP `DeleteUser` | **async + durable reconcile** | `kc_cleanup_pending=true`, PI-9 reconciler converges |
| Session revoke (AUTH-8) | RP `RevokeUserSessions` | **fail-open (TTL backstop)** | O&M commits, TTL bounds effective cutoff |
| Plan-defaults on I-8 miss | Catalog Service `PlanByCode` (via `catalogReader`, `om:plans` cache) | **fail-open (swallowed)** | I-8 proceeds with no plan-default merge — never a hard failure on the hot path |

Three fail-closed calls (invite, delegation, removal/resolution) stop completing during the respective dependency's outage; none corrupts state. Fail-open + durable reconcile chosen for security-critical and high-value paths.

---

## Out of scope

This service does **not** handle:

| Concern | Where it lives |
|---------|---------------|
| Authentication, JWT issuance, MFA enforcement | Keycloak |
| Realm provisioning, Keycloak Admin API calls | Realm Provisioner |
| Display identity, availability presentation, signature, OOO expiry sweep | User Profile |
| Workflow templates, `assignee_overrides` state | Workflow Service |
| Tender content, bids | Tender Service |
| Audit records | Audit Log |
| Metered quota consumption (tokens, requests) | Usage & Metering (HLD §10.6) |
| Pricing, currency, seat enforcement past grace | Billing (`default_currency` is deliberately not stored here — §16 A32(b) / T-3) |
| Notification delivery | Notification Service |

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, extension recipes, test requirements, branch naming, commit style, and the PR checklist.

Detailed reference docs in `.claude/` — consumed by Claude Code when working in this repo:

| Document | Description |
|----------|-------------|
| [`.claude/CLAUDE.md`](.claude/CLAUDE.md) | Service pitch, key files, ownership boundaries |
| [`.claude/architecture.md`](.claude/architecture.md) | Clean Architecture layout, shared library integration |
| [`.claude/database-schema.md`](.claude/database-schema.md) | 15-table schema, RLS/tenant/seat invariants |
| [`.claude/api-caching-events.md`](.claude/api-caching-events.md) | Endpoint catalogue, cache keys, event catalogue, EVT-14/15/16 |
| [`.claude/request-flows.md`](.claude/request-flows.md) | Provisioning, delegation, invite→accept, GDPR |
| [`.claude/operations.md`](.claude/operations.md) | Security, observability, deployment, CI/CD, dependency matrix |

Full LLD (v1.61 Draft, 4799 lines) lives at `iam-lld-org-membership-Final.md` in the repo root.

For the detailed architecture narrative with diagrams see [ARCHITECTURE.md](ARCHITECTURE.md).

---

## License / ownership

BCBP Solutions FZC LLC — internal platform service. Not for external distribution.
