# iam-org-membership

The Org & Membership (Core) Service — the **organizational/business system of record** in the IAM subsystem. It owns tenants (plan, subscription lifecycle, trial/cancellation/suspension timers, MFA freshness), department activation, tenant memberships and elevated tenant-level roles, department-level role grants, the two-step invite→accept staging flow, and per-tenant feature-flag overrides. It is the hot-path source of truth AuthZ Enrichment reads on every authenticated request via I-8.

**Repository:** `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership`
**Module:** Go 1.26.6 · private module · deployed as two binaries from one image (`cmd/server`, `cmd/reconciler`)
**Design:** Refines the IAM HLD (`IAM HLD v1.41`, §5.6); LLD **v2.3** (`docs/lld/iam-lld-org-membership-service.md`) — where LLD and HLD disagree, HLD is authoritative. This repo is post-decomposition: departments/plans catalog ownership moved to Catalog / Admin Config Service, SAML group→dept/role mapping moved to Group Mapping / JIT Config Service, tender ACL overlays moved to Tender ACL Service, and delegation grants + OOO coordination moved to Delegation Service (ADR-0007 + ADR-0008) — this repo is what's left after all four extractions.

---

## Mental model

| This service owns | It does NOT own |
|---|---|
| `tenants` — plan (ENUM), subscription status, trial metadata, realm identity/type/shard, MFA freshness, locale, `local_accounts_enabled`, licensed seats, ownerless/overage/realm-sync markers, `suspension_source` (`billing_lapse`\|`operator`) | Credentials, MFA enforcement, JWT issuance (Keycloak) |
| `tenant_departments` — per-tenant activation junction only | The global department catalog itself (Catalog / Admin Config Service) |
| `tenant_memberships` — lifecycle only, no role data | Display identity, signature, OOO presentation flag (User Profile) |
| `tenant_roles` — `tenant_owner`/`tenant_admin`/`tender_admin` only; `member` derived at read time, never persisted (TR-7) | Plan entitlement catalog (Catalog / Admin Config Service) |
| `dept_memberships` — user↔department↔role-level; `dept_role_labels` (tenant-customizable display labels) | Group→dept/role JIT mapping tables (Group Mapping / JIT Config Service) |
| `pending_invitations` — two-step invite→accept staging (the one PII exception: email + full name until acceptance) | Tender ACL overlay `tender_acl_entries` (Tender ACL Service) |
| `tenants.plan`/`tenants.feature_flags` — per-tenant delta only; effective set = `planDefaults(plan) ∪ feature_flags` at read time | Delegation record/lifecycle/policy/events `delegations` (Delegation Service) |
| — | Realm/Keycloak Admin API mutations (Realm Provisioner — this service **never** calls Keycloak) |
| — | Workflow template authoring or `assignee_overrides` state (Workflow Service — we validate + emit, never persist) |
| — | Audit records (Audit Log Service — subscribes to `membership-audit-q` with no filter, the only real audit mechanism in this codebase) |
| — | Metered resource consumption / quota enforcement (Usage & Metering) |

Realm Provisioner owns Keycloak realm/user/IdP administration and calls this service over the mesh (I-1/I-2) or polls it (I-16); this service owns the organizational/business model and never holds a Keycloak admin credential of any kind.

**Core's only remaining delegation touchpoint:** a synchronous delegate-impact gate on user removal (via `port.WorkflowClient` — talks to the *Workflow* Service, unaffected by the decomposition) plus a synchronous department-scope precision lookup on the admin dept-membership Assign/Remove path via `port.DelegationCheckClient`, which degrades to tenant-wide impact scoping (still correct, less precise) on a Delegation Service outage — never a hard failure.

---

## Why this service exists

Every authenticated request in the platform needs one fast, authoritative answer to "what can this user do in this tenant right now" — role grants, department memberships, subscription state, effective feature flags. Centralizing that projection behind one service (I-8, the AuthZ Enrichment hot path) means:

- AuthZ Enrichment has exactly one place to ask, with one cache-invalidation contract (`om:memberships`), instead of assembling the answer from four different services' own tables.
- Seat-cap enforcement (SEAT-1), last-owner protection (TM-8), and optimistic-lock concurrency (CONC-1..4) are enforced once, transactionally, rather than re-implemented per caller.
- The tenant lifecycle projection (`active → past_due → cancelled → suspended → offboarded`) has one authoritative implementation reacting to Billing's and Realm Provisioner's events, so "is this tenant currently writable" always has one correct answer (`read_only` derived at read time from `subscription_status`).
- The four service extractions (ADR-0007/ADR-0008) each moved a genuinely separable concern (catalog config, group-mapping JIT config, tender ACL overlays, delegation) to its own service and database, while this service kept the one thing that can't be split without breaking the I-8 hot path: the tenant/membership/role core, in one Postgres database, behind one RLS boundary.

This is enforced structurally: `internal/core/service` depends only on `internal/core/port` interfaces — never on a vendor DB type directly (`port.AuthZRepository` is the seam I-8's four-table join sits behind) — and `.go-arch-lint.yml` encodes the full dependency-direction ruleset CI checks on every PR.

---

## API overview

**36 active endpoints** across three route prefixes. Source of truth: `internal/adapter/inbound/http/router.go`; the generated REST contract is `docs/swagger/swagger.yaml` (`make swag`), not hand-authored. Retired route IDs (below) are fully unregistered — not `410 Gone` — and never reused.

**Middleware chain:** `gincommon.DefaultMiddlewares` (`PanicRecovery → RequestID → Tracing → CorrelationHeaders → Metrics → Logging → RequireAuth → ContextMiddleware`) plus a GUC-bridge middleware that binds `app.tenant_id` transaction-locally (RLS-6). The service trusts gateway-injected headers (`x-user-id`, `x-tenant-id`, `x-tenant-roles`) under mesh mTLS — no JWT parsing.

### Public routes (21) — `/api/v1/*`

| # | Method & Path | Purpose |
|---|---|---|
| P-1 | `GET /tenants/:id` | Tenant details (name, plan, locale, `mfa_freshness_seconds`) |
| P-2 | `PATCH /tenants/:id` | Update name, locale, `local_accounts_enabled`, `mfa_freshness_seconds` |
| P-3 | `GET /tenants/:id/departments` | List active departments for tenant |
| P-4 | `GET /tenants/:id/members` | List tenant members, cursor-paginated |
| P-5 | `GET /tenants/:id/members/:user_id` | Single member record |
| P-6 | `POST /tenants/:id/members` | **Invite** — two-step invite→accept, seat-cap gated (`409 seat_limit_reached`) |
| P-7 | `PATCH /tenants/:id/members/:user_id` | Suspend/reactivate a membership (`status` only) |
| P-8 | `DELETE /tenants/:id/members/:user_id` | Remove user — gated by the delegate-impact pre-check |
| P-9 | `GET /tenants/:id/departments/:dept_id/members` | List dept members at each role level |
| P-10 | `PUT /tenants/:id/departments/:dept_id/members/:user_id` | Assign to department at role level (a level decrease is delegate-impact gated) |
| P-11 | `DELETE /tenants/:id/departments/:dept_id/members/:user_id` | Remove from department (delegate-impact gated) |
| P-12 | `GET /tenants/:id/roles` | List tenant role catalog |
| P-13 | `PATCH /tenants/:id/roles/:role_code` | Update a role's display label |
| P-24 | `POST /tenants/:id/departments` | Activate a global-catalog department for this tenant |
| P-25 | `PATCH /tenants/:id/departments/:dept_id` | Deactivate/reactivate the tenant's department activation |
| P-26 | `POST /tenants/:id/users/:user_id/removal-resolution` | Resolve a blocked removal — `replace_delegate` or `stop_workflows` |
| P-27 | `GET /tenants/:id/seat-usage` | Active-user count, `licensed_seats`, `over_cap` |
| P-28 | `PUT /tenants/:id/members/:user_id/roles` | Full-replacement reconcile of tenant-level roles (`422 last_owner_removal` guard) |
| P-30 | `GET /tenants/:id/invitations` | List outstanding pending invitations |
| P-31 | `DELETE /tenants/:id/invitations/:invitation_id` | Revoke a still-pending invitation |
| P-34 | `POST /tenants/:id/members/:user_id/reset-mfa` | Reset a member's MFA via Realm Provisioner — **fail-closed** |

*Retired, never reused:* P-14/15/16/17/29 (group-mapping admin CRUD → Group Mapping Service), P-18/19/20/32/33 (delegation → Delegation Service), P-21/22/23 (tender ACL → Tender ACL Service).

### Internal routes (13) — `/api/v1/internal/*`, mesh-mTLS + NetworkPolicy only

| # | Method & Path | Caller | Purpose |
|---|---|---|---|
| I-1 | `POST /tenants` | Realm Provisioner / Signup BFF | Provision a new tenant row |
| I-2 | `PATCH /tenants/:id` | Realm Provisioner | Set `realm_id`, `realm_type='dedicated'`, `keycloak_shard` |
| I-3 | `POST /tenants/:id/members` | Event Consumer | Add membership from Keycloak `REGISTER` — also the invitation-acceptance path |
| I-4 | `PATCH /tenants/:id/members/:user_id` | Event Consumer | Update membership status from a Keycloak lifecycle event |
| I-5 | `DELETE /tenants/:id/members/:user_id` | Event Consumer | Soft-delete on Keycloak `USER_DELETE` — delegate-impact gated |
| I-8 | `GET /users/:id/memberships` | **AuthZ Enrichment (hot path)** | Full membership context for header injection — four-table join |
| I-9 | `GET /tenants/:id/locale` | LLM Service | Tenant default locale for prompt assembly |
| I-10 | `POST /tenants/:id/dept-memberships` | Event Consumer | SAML group assertion → dept memberships + additive tenant-role grants |
| I-11 | `GET /tenants/:id/seat-usage` | Billing | Same handler as P-27; pre-check before seat reduction |
| I-13 | `POST /tenants/:id/tenders/:tender_id/assignee-override` | Workflow Service | Validate-and-emit for node reassignment — persists nothing |
| I-14 | `GET /tenants/:id/mfa-freshness` | AuthZ Enrichment | Authoritative read for the Approver step-up gate |
| I-15 | `GET /tenants/:id/members/:user_id/exists` | Tender ACL Service, Delegation Service (×2) | Grant-time membership-existence check, replaces a composite FK lost across the DB split |
| I-16 | `GET /subscription-lapses` | Realm Provisioner (RP-C3 lapse sweep) | Cross-tenant bulk read of tenants past their cancellation grace period |

*Retired, never reused:* I-6/I-7 (quotas → Usage & Metering), I-12 (tender-ACL check → Tender ACL Service).

### Operator routes (2) — `/api/v1/operator/*`, `platform_operator` only

| # | Method & Path | Purpose |
|---|---|---|
| O-4 | `PATCH /operator/tenants/:id/feature-flags` | Full-replacement of a tenant's feature-flag override delta |
| O-7 | `POST /operator/tenants/:id/reassign-owner` | Recover an ownerless tenant; grants `tenant_owner` to an existing active member |

*Retired, never reused:* O-1/2/3/5/6 (department/plan catalog admin → Catalog Service).

---

## Input validation

Domain-rule failures are raised as a `*domain.DomainError` wrapping one of the sentinel error values in `internal/core/domain/errors.go`; `HandleError` (`internal/adapter/inbound/http/middleware.go`) maps `DomainError.Code` to a frozen HTTP status. A raw `*pgconn.PgError` that escapes the repository layer untranslated is classified by SQLSTATE class — `08`/`53` via `pgcommon.IsConnectionException`/`IsInsufficientResources` (added in `platform-pgcommon` v1.3.0), `57`/`58` via a small local fallback (pgcommon has no dedicated helper for those two yet) — into `503 db_unavailable`; anything else falls back to `500 internal_error` and is logged.

```json
{
  "error": "seat_limit_reached",
  "status": 409,
  "licensed_seats": 10,
  "active_users": 9,
  "pending_invitations": 1
}
```

`DomainError.Details` merges into the flat envelope at the top level (`record_version`, `active_workflows`, `workflow_ids`, `allowed_actions`, `licensed_seats`, `retry_after_seconds`, ...) rather than a nested `details` object.

### Notable validation rules

| Rule | Enforcement |
|---|---|
| `tenant_id` in the request body/path is never trusted for authorization | Identity comes exclusively from gateway-injected headers; RLS returns zero rows on any mismatch (API-1/API-2) |
| A tenant-role reconcile (P-28) can never assign `member` | `chk_tr_no_member` CHECK, backstopped by service-layer validation (`400 invalid_role`) — `member` is derived at read time, never stored (TR-7) |
| A `tenant_owner`-affecting mutation can never leave zero active owners | `422 last_owner_removal` (TM-8), serialized via `SELECT ... FOR UPDATE` on the `tenants` row (TM-13) so two concurrent removals can't both pass |
| Seat cap is enforced transactionally, not just pre-flight | `SELECT ... FOR UPDATE` on `tenants` under the same tx that counts `active + pending` against `licensed_seats` (SEAT-1) |
| Optimistic locking | 7 of 8 tables carry `record_version` (`touch_row` DB trigger); a version mismatch returns `409 optimistic_lock_conflict` with the current version in the body |
| Invitation rate limiting | Per-email cooldown (`INVITE_REINVITE_COOLDOWN_MINUTES`, `429 reinvite_too_soon`) and per-tenant hourly ceiling (`INVITE_MAX_PER_TENANT_PER_HOUR`, `429 invite_rate_limited`) — both checked pre-flight, before any Realm Provisioner call |

---

## Architecture

Clean Architecture — dependencies point inward; outer layers never import inner layers. Full layer diagrams, package dependency graph, and shared-library integration are in **[`.claude/architecture.md`](.claude/architecture.md)**; standalone Mermaid diagrams live in **[`docs/architecture/`](docs/architecture/README.md)**.

```
iam-org-membership/
├── cmd/
│   ├── server/                        # HTTP composition root: pool+GUC wiring, migrations, outbox runner, SQS consumer, 4 metric-exporter goroutines
│   └── reconciler/                    # Single binary, --job=<name>; jobs/ holds the 7 CronJob entry points
├── internal/
│   ├── core/
│   │   ├── domain/                    # Entities, value objects, DomainError catalogue — no external deps
│   │   ├── port/                      # TenantRepository, MembershipRepository, AuthZRepository (NEW), WorkflowClient, RealmProvisionerClient, CatalogAdminClient, GroupMappingClient, DelegationCheckClient
│   │   └── service/                   # MembershipService, TenantService, AuthZService, ProvisioningService, OperatorService, InvitationService, CatalogService, GroupMappingService, SubscriptionLapseService
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                  # Gin handlers (P-*/I-*/O-*), DTOs, middleware, router.go, Swagger UI
│       │   └── consumer/              # SQS consumer: tenant-orgm-q, billing-orgm-q (EVT-14/15/16 guards)
│       └── outbound/
│           ├── postgres/              # Repository impls + single consolidated 000000_initial_schema migration
│           ├── valkey/                # Cache adapter (go-redis/v9) — advisory only
│           ├── eventbus/              # RoutingPublisher (two topics) + outbox runner + embedded event schemas
│           ├── workflow/               # Workflow Service HTTP client (§8.8 delegate-impact)
│           ├── realmprovisioner/       # Realm Provisioner HTTP client — CreateInvitedUser/DeleteUser/PatchRealmConfig/RevokeUserSessions/ResetMFA
│           ├── catalogadmin/           # Catalog Service HTTP client — departments/plans (NOT fail-open)
│           ├── groupmappingclient/     # Group Mapping Service HTTP client — I-10 JIT resolution (fails open)
│           ├── delegationcheck/        # Delegation Service HTTP client — §8.8.4 dept-scope precision lookup (fails open)
│           ├── httpx/                  # Shared otelhttp-instrumented http.Client factory — scaffolded, NOT yet wired into any of the 5 outbound clients above (each still builds its own http.Client + local propagateTraceparent)
│           └── metrics/                # iam_*-prefixed Prometheus counters/gauges/histograms
├── pkg/requestctx/                    # Typed RequestContext{UserID, TenantID, Roles, ClientIP, UserAgent}
├── api/
│   └── asyncapi.yaml                  # AsyncAPI — iam.membership.events (12) + iam.tenant.events (2)
├── docs/
│   ├── lld/                           # LLD v2.3 — §16 open-question register, §17 error taxonomy
│   ├── swagger/                       # Generated REST contract (make swag) — not hand-authored
│   └── runbook-schema-registry.md     # Schema-governance operator runbook
├── deploy/                            # Helm chart, monitoring alerts, IAM policy
├── .githooks/pre-commit                # tidy + fmt-check + vet + lint; installed via `make setup`
└── test/                              # test/unit (black-box), test/postgres (RLS + service integration), test/integration (SNS/SQS), test/e2e, test/dbseed
```

### Dependency rules (enforced by `go-arch-lint` in CI)

| Component | May depend on |
|---|---|
| `domain` | Nothing internal |
| `port` | `domain` only |
| `service` | `domain`, `port`, `requestctx`, `observability` (a documented cross-cutting leaf) |
| `adapters_inbound` (http/consumer) | `service`, `port`, `domain`, `requestctx`, `observability`, `apispec` |
| `adapters_outbound` (postgres/valkey/eventbus/workflow/realmprovisioner/catalogadmin/groupmappingclient/delegationcheck) | `port`, `domain`, `eventschema`, `observability`, `httptransport` |
| `httptransport` (`internal/adapter/outbound/httpx` — scaffolded, not yet imported by any outbound client) | Nothing internal |
| `reconciler_jobs` (`cmd/reconciler/jobs`) | `domain`, `port`, `observability`, `requestctx` |
| `cmd` (server/reconciler) | Everything above |

Plus **RLS-6**, CI-enforced by grep: every write to `app.tenant_id` must be `SET LOCAL` (transaction-scoped), never a session-scoped `SET`, so a value can never leak across a pooled PgBouncer connection.

### Storage and messaging

| Concern | Technology | Notes |
|---|---|---|
| **Primary store** | PostgreSQL 17, database `org_membership` on shared RDS (Multi-AZ, PgBouncer transaction pooling) | 8 tables, `FORCE ROW LEVEL SECURITY` on all 7 tenant-scoped tables; GUC `app.tenant_id` bound transaction-locally |
| **Cache** | Valkey (Redis-compatible) via `go-redis/v9` | Advisory-only (CACHE-2/9) — a miss or outage falls through to Postgres, never a hard failure |
| **Events (outbound)** | AWS SNS + transactional outbox (`platform-events`) | Two topics via `RoutingPublisher`: `iam.membership.events` (12 event types) and `iam.tenant.events` (2 — `TenantCreated`/`TrialStarted`) |
| **Events (inbound)** | AWS SQS, 2 queues | `tenant-orgm-q`, `billing-orgm-q`, each with its own `-dlq`; EVT-14 recency guard + EVT-15 future-time clamp + EVT-16 tenant-state relay |
| **Schema registry** | AWS Glue, one registry per topic (SCHEMA-7) | `iam-membership-events`, `iam-tenant-events`; governed by `platform-schemagov` |

### Shared library dependencies

| Library | Version | Purpose |
|---|---|---|
| `platform-gincommon` | v1.3.0 | HTTP middleware, Zap logging, OTel tracing, Prometheus metrics |
| `platform-events` | v1.4.0 | Transactional outbox, SNS `RoutingPublisher`, SQS consumer |
| `platform-pgcommon` | v1.3.0 | pgx/v5 pool, RLS GUC injection, migrations, error helpers (`IsConnectionException`/`IsInsufficientResources`/`IsPgError`) |

---

## Integrating with other services

### 1. Prerequisites

Every caller must run on the internal service mesh (mTLS + NetworkPolicy) for `/api/v1/internal/*`, or forward gateway-validated `x-user-id`/`x-tenant-id`/`x-tenant-roles` headers for `/api/v1/*`. There is no JWT parsing in this service — the gateway/mesh has already done it.

### 2. Realm Provisioner — bidirectional

| Direction | Call | Notes |
|---|---|---|
| RP → this | `POST /tenants` (I-1), `PATCH /tenants/:id` (I-2) | Tenant provisioning / realm-id set |
| this → RP | `CreateInvitedUser`, `DeleteUser`, `PatchRealmConfig`, `RevokeUserSessions`, `ResetMFA` | Fail-closed on invite/MFA-reset; fail-open + durable reconcile on config/session |
| RP → events → this | `TenantRealmReady`, `TenantConverted`, `TenantSuspended`, `TenantOffboarded`, `TrialExpired`, `TrialReactivated`, `TenantReactivated` on `iam.tenant.events` (`tenant-orgm-q`) | RP is the sole producer on this topic besides this service's own `TenantCreated`/`TrialStarted` |
| this → events → RP | `TrialStarted{tenant_id, plan, trial_ends_at}` on `iam.tenant.events` | RP seeds its own local trial-expiry sweep off this — no polling either direction |
| RP → this | `GET /subscription-lapses` (I-16) | RP's subscription-lapse sweep (RP-C3) polls this instead of tracking `cancelled_at` itself — deliberately pull, not push, since a cancellation is reversible (`TenantReactivated`) in a way trial-expiry never is |

### 3. Billing — events only

Billing produces on `billing-orgm-q`: `TenantSeatsChanged`, `TenantPlanChanged`, `TenantPaymentPastDue`, `TenantSubscriptionCancelled`, `TenantReactivated`. This service reacts — it never calls Billing synchronously except serving Billing's own pre-check read (I-11, same handler as P-27).

### 4. AuthZ Enrichment — the hot path

`GET /internal/users/:id/memberships` (I-8), SLO 15 ms cache hit / 30 ms miss. Returns the full membership projection: status, plan, `subscription_status`, derived `read_only`, elevated + derived-`member` roles, department memberships with role levels, and the effective feature-flag list. Cached under `om:memberships:{tenant}:{user}` with 300 s ± 30 s jitter; evicted on every write via the `membership-authz-q` SQS subscription to this service's own outbox events.

### 5. Workflow Service

`GetDelegateImpact`/`ReassignDelegate`/`CancelByDelegate` (§8.8, this service → Workflow, fail-closed on removal); `POST /tenants/:id/tenders/:tender_id/assignee-override` (I-13, Workflow → this, validate-and-emit, persists nothing).

### 6. Tender ACL Service / Delegation Service — grant-time existence check

Both extracted services lost a composite FK into `tenant_memberships` across the database split. `GET /tenants/:id/members/:user_id/exists` (I-15) replaces it: `{active, tenant_membership_id}`, never `404` — absence is a valid answer, not an error.

### 7. Subscribing to SNS events

This service publishes to two topics via a transactional outbox — 12 events on `iam.membership.events`, 2 on `iam.tenant.events`.

| Event | Topic | Trigger |
|---|---|---|
| `DepartmentMembershipGranted`/`Revoked`/`LevelChanged` | membership | Department assignment changes |
| `TenantRoleGranted`/`Revoked` | membership | Elevated tenant-role grant/revoke (P-28, or additive JIT via GTRM-4) |
| `MembershipRevoked` | membership | User removed from tenant — shared cascade signal for Delegation + Tender-ACL's own async cascades |
| `TenderAssigneeOverridden` | membership | I-13 validate-and-emit |
| `MFAReset` | membership | P-34 — sole audit record for an MFA reset, consumed by Audit Log's catch-all |
| `TenantSeatOverageStarted`/`Resolved` | membership | Seat usage crosses/returns from over `licensed_seats` |
| `TenantStateChanged` | membership | Relay of a consumed lifecycle event that actually changed `status`/`plan` — lets Workflow subscribe to one topic instead of three |
| `TenantMembershipsPurged` | membership | Tenant offboarding — Delegation/Tender-ACL/Group-Mapping run their own cascade-deletes |
| `TenantCreated` / `TrialStarted` | tenant | The only two events this service produces on `iam.tenant.events` |

**Idempotency:** record the envelope `id` (UUID v7) against your own consumer name before committing any side effect — delivery is at-least-once. **Ordering:** SNS does not guarantee delivery order; this service's own consumer handles it via an `EVT-14` recency guard keyed on `tenants.last_event_at` under a row lock.

### 8. Handling errors

Every non-2xx response is the flat envelope shown above. See `docs/lld/iam-lld-org-membership-service.md` §17 for the complete error taxonomy.

### 9. Rate limits

No generic per-caller/per-endpoint rate limiter exists in `internal/adapter/inbound/http/` — inbound throttling, if any, is the gateway's concern. The only request-level throttling in this codebase is invite-specific business logic: PI-11 (per-email cooldown) and PI-12 (per-tenant hourly ceiling), both pre-flight-checked before P-6 ever calls Realm Provisioner.

### 10. Background reconcilers you may observe

7 CronJobs, one Helm manifest, each `cmd/reconciler --job=<name>` against the same image:

| CronJob (`--job=`) | Schedule | Purpose |
|---|---|---|
| `invitation-expiry` | `*/5 * * * *` | Sweeps `pending_invitations` past `expires_at` → `expired`, sets `kc_cleanup_pending` |
| `invitation-kc-cleanup` | `*/10 * * * *` | Idempotent `RealmProvisioner.DeleteUser` for rows with `kc_cleanup_pending` |
| `realm-config-sync` | `*/10 * * * *` | Retries `realm_sync_pending` tenants against Realm Provisioner (T-15 Option A) |
| `seat-overage-reconcile` | `0 */6 * * *` | Self-heals `overage_since`/`grace_ends_at` for any missed set/clear |
| `trial-cleanup` | `0 2 * * *` | Hard-deletes trial tenants past grace beyond `trial_expired` |
| `outbox-prune` | `0 3 * * *` | Prunes published `outbox_events` past `OUTBOX_RETENTION_DAYS` |
| `processed-events-prune` | `0 4 * * *` | Prunes `processed_events` rows past `PROCESSED_EVENTS_TTL_DAYS` |

The 4 business-metric gauges (`iam_org_membership_tenant_ownerless`, `iam_org_membership_realm_sync_pending`, `iam_org_membership_seat_overage_active`, `iam_org_membership_pending_invitations_stale`) run as **ticker goroutines inside `cmd/server`**, not CronJobs.

---

## Local development

### Prerequisites

- Go 1.26.6+
- Docker (Postgres, PgBouncer, Valkey, floci — `make docker-up`)
- `GOPRIVATE=github.com/BCBP-SOLUTIONS-FZC-LLC/*` (`GONOSUMDB` too) and an SSH key registered with the BCBP org

### Setup

```bash
git clone https://github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership
cd iam-org-membership
make setup       # copies .env-example → .env (run once before anything else)
make tidy        # go mod tidy
make docker-up   # start PostgreSQL + PgBouncer + Valkey + floci
make run         # start the server on :8080 (metrics on :9090; kills the port first)
```

### Common commands

| Command | Description |
|---|---|
| `make setup` | Copy `.env-example` → `.env` |
| `make tidy` / `make fmt` / `make fmt-check` / `make vet` | Go basics; `fmt-check` mirrors CI, does not modify files |
| `make lint` | `golangci-lint` via `go tool` |
| `make mod-verify` | `go mod verify` |
| `make vuln-check` | `govulncheck ./internal/...` |
| `make test` | Unit + postgres + integration in parallel (Docker required; e2e is separate) |
| `make test-ci` | Same, with `-race` + merged coverage (used in CI) |
| `make test-unit` | Unit tests only, no Docker |
| `make test-postgres` | Postgres + RLS integration (testcontainers-go); every test calls `t.Parallel()`, capped at `TEST_POSTGRES_PARALLEL` (default 4) |
| `make test-integration` | Cross-layer (SNS/SQS/Glue via floci, testcontainers) |
| `make test-e2e` | End-to-end tests |
| `make test-smoke` | CI-only Docker image gate — size ≤200MB + startup check, not a functional test |
| `make race` | All three suites with `-race`, no coverage merge |
| `make run` | Run the server locally (sources `.env`, kills port 8080 first) |
| `make build` | Compile both binaries to `bin/` |
| `make cover` / `make cover-func` | Coverage HTML report / per-function summary |
| `make ci` | `tidy` + `fmt-check` + `vet` + `lint` + `test-ci` + `build` |
| `make docker-up` / `make docker-down` | Start/stop PostgreSQL + PgBouncer + Valkey + floci |
| `make schema-verify` | Pre-deploy check: Glue registry schema names/versions vs `api/asyncapi.yaml` + embedded JSON schemas |
| `make swag` / `make swag-check` | Regenerate / verify freshness of the Swagger REST contract |
| `make clean` | Remove `bin/` artefacts and coverage files |

### Running a single test

```bash
go test ./test/unit/... -run TestMembership_RemovalResolution_ReplaceDelegate_HappyPath -v
go test ./test/postgres/... -tags=integration -run TestRLS_Case5_NoCrossTenantLeakAcrossPool -v
```

### Calling the API locally

```bash
# After make docker-up and make run:

# I-1: provision a tenant (internal route — normally called by Signup BFF/Realm Provisioner)
TENANT_ID=$(python3 -c 'import uuid; print(uuid.uuid4())')
curl -i -X POST http://localhost:8080/api/v1/internal/tenants \
  -H "Content-Type: application/json" \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: $TENANT_ID" -H "x-tenant-roles: iam-system" \
  -d "{\"tenant_id\":\"$TENANT_ID\",\"slug\":\"acme\",\"name\":\"Acme Corp\",\"plan\":\"starter\",\"owner_user_id\":\"$(python3 -c 'import uuid; print(uuid.uuid4())')\"}"

# P-1: read tenant details
curl -i http://localhost:8080/api/v1/tenants/$TENANT_ID \
  -H "x-user-id: <owner-uuid>" -H "x-tenant-id: $TENANT_ID" -H "x-tenant-roles: tenant_owner"

# P-6: invite a member
curl -i -X POST http://localhost:8080/api/v1/tenants/$TENANT_ID/members \
  -H "Content-Type: application/json" \
  -H "x-user-id: <owner-uuid>" -H "x-tenant-id: $TENANT_ID" -H "x-tenant-roles: tenant_owner" \
  -d '{"email":"newmember@example.com","full_name":"New Member"}'
```

### Developer tools

Interactive Swagger UI (custom BCBP theme, Try-it-out) is served at `/swagger/*any`, gated the same way as the docs surface below.

```bash
open http://localhost:8080/swagger/index.html
```

`DOCS_ENABLED`/`DOCS_AUTH_TOKEN` gate the docs surface in production; both are always mounted outside production.

---

## Testing domain events locally

Every mutating write publishes a domain event through a **transactional outbox → SNS → SQS** pipeline.

### How the pipeline works

```
HTTP write / reconciler job
    │
    ▼
service layer  ──(same tx)──▶  outbox_events (Postgres)
                                      │
                               outbox runner (OUTBOX_POLL_INTERVAL, 500 ms)
                                      │
                                      ▼
                     SNS: iam.membership.events / iam.tenant.events   (floci)
                                      │
                    SNS fan-out to downstream SQS queues (local dev only)
```

An outbox insert is atomic with the business write — a `2xx` response guarantees an `outbox_events` row exists.

### Step 1 — Start infrastructure

```bash
make docker-up
```

`scripts/init-floci.sh` runs automatically and provisions the two SNS topics, the inbound `tenant-orgm-q`/`billing-orgm-q` queues, and both Glue registries + all 14 schemas — floci includes Glue Schema Registry in its free tier, so `GLUE_REGISTRY_*_NAME` is set by default in `.env-example` and the real Glue wire-format codec runs locally instead of falling back to `NoopCodec`.

### Step 2 — Verify SNS/SQS/Glue exist

```bash
docker compose exec floci aws --region ap-south-1 sns list-topics
docker compose exec floci aws --region ap-south-1 sqs list-queues
docker compose exec floci aws --region ap-south-1 glue list-schemas --registry-id RegistryName=iam-membership-events
```

### Step 3 — Trigger an event and inspect the outbox

```bash
make run
# ... issue a P-6 invite or I-1 provision call (see above) ...

docker compose exec postgres psql -U org_membership_app -d org_membership -c \
  "SELECT id, event_type, published_at IS NOT NULL AS published, attempts
   FROM outbox_events ORDER BY created_at DESC LIMIT 20;"
```

### Troubleshooting events

| Symptom | Likely cause | Fix |
|---|---|---|
| `outbox_events` row never gets `published_at` set | Topic ARN mismatch, or outbox runner not started | Re-check `.env`'s `SNS_TOPIC_*_ARN` against `awslocal sns list-topics` |
| `outbox_events` empty after a write | Row was published and pruned, or the write never committed | Re-check the HTTP response code — a `2xx` guarantees the row was committed |
| Messages keep reappearing after `receive-message` | Normal — SQS visibility timeout, not deletion | Use `delete-message` |

---

## Testing

### Canonical tests (do not break)

- **`test/postgres/rls_test.go`** — the canonical RLS cases, including **Case 5**: no cross-tenant GUC leak across a pooled PgBouncer connection. CI additionally greps for any non-`LOCAL` `SET app.tenant_id` as a forbidden pattern.
- **`test/postgres/subscription_lapse_test.go`** — I-16's cross-tenant, BYPASSRLS-bound query returns only tenants past grace and drops a tenant off the next poll once suspended (self-idempotence).
- **`test/unit/authz_service_test.go`** — I-8's business-logic composition (TR-7 derived-role injection, PLAN-6 feature-flag merge, graceful degradation on a Catalog Service outage), unit-tested with no Docker at all now that `AuthZService` depends on `port.AuthZRepository` instead of `*pgcommon.Pool` directly.

### Coverage

`.github/scripts/coverage-gate.sh` reads `go tool cover -func=coverage.out`'s total and fails below `COVERAGE_THRESHOLD` (default **95%**, not overridden in this repo's `validate-test.yml`). Coverage is measured over `./internal/...` and `./pkg/...` (`COVER_PKG_LIST` in the Makefile), merged across the unit/postgres/integration suites via `scripts/merge_coverage.py` (max-count strategy). The current merged total is **98.4%**, comfortably above the enforced floor. `internal/adapter/inbound/http` sits at 100%; `internal/core/domain`, `internal/core/port`, `pkg/requestctx`, and `internal/adapter/outbound/httpx` are all at 100% as well; `internal/core/service` and `internal/adapter/outbound/postgres` are both above 99%. The handful of packages below 95% (`catalogadmin`, `realmprovisioner`, `consumer`) have only a few residual statements each — mostly defensive branches (e.g. a `json.Marshal` error path on an always-marshalable struct) that are impractical to exercise without contriving unrealistic inputs, not real gaps.

---

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `APP_NAME` / `APP_ENV` / `APP_PORT` | `iam-org-membership` / `dev` / `8080` | Service identity, env-gated behavior, HTTP listen port |
| `METRICS_PORT` | `9090` | Dedicated `/metrics` listener, separate from `APP_PORT` |
| `DATABASE_URL` or `PG_HOST`/`PG_PORT`/`PG_USER`/`PG_PASSWORD`/`PG_DBNAME`/`PG_SSLMODE` | — | App pool DSN — required one way or the other |
| `PG_MAX_CONNS` / `PG_MIN_CONNS` / `PG_SLOW_QUERY_THRESHOLD` | `20` / `0` / `200ms` | Pool sizing + slow-query log threshold |
| `PG_BOUNCER_MODE` | `true` (dev) | Transaction-scoped GUC injection when behind PgBouncer |
| `MIGRATION_DATABASE_URL` | — | **Required.** Direct (non-PgBouncer) DSN — migrations acquire a session-scoped advisory lock |
| `SYSTEM_DATABASE_URL` | — | BYPASSRLS pool for reconciler jobs, business-metric exporters, and I-16's cross-tenant read — must target `org_membership_migrator` in production |
| `VALKEY_URL` | `localhost:6380` (dev) | Advisory cache; `rediss://` required outside dev |
| `SNS_TOPIC_MEMBERSHIP_ARN` / `SNS_TOPIC_TENANT_ARN` | — | **Required outside dev.** The two `RoutingPublisher` topic ARNs |
| `SQS_TENANT_ORGM_QUEUE_URL` / `SQS_BILLING_ORGM_QUEUE_URL` | — | Inbound lifecycle-event queues |
| `GLUE_REGISTRY_MEMBERSHIP_NAME` / `GLUE_REGISTRY_TENANT_NAME` | — | Unset → `NoopCodec` (plain JSON) |
| `AWS_REGION` / `AWS_ENDPOINT_URL` / `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | `ap-south-1` / — | floci override in dev; IRSA in production |
| `OUTBOX_POLL_INTERVAL` / `OUTBOX_BATCH_SIZE` / `OUTBOX_MAX_ATTEMPTS` / `OUTBOX_DRAIN_TIMEOUT` / `OUTBOX_PUBLISH_CONCURRENCY` / `OUTBOX_PUBLISH_TIMEOUT` / `OUTBOX_STARTUP_JITTER` / `OUTBOX_CLAIM_LEASE_DURATION` | `500ms` / `50` / `5` / `30s` / `4` / `10s` / `2s` / `10m` | Outbox runner tunables |
| `PROCESSED_EVENTS_TTL_DAYS` | `8` | Consumer dedup retention — deliberately > the 7-day SQS message lifetime (PE-1) |
| `CACHE_TTL_SECONDS` | `300` | Base TTL for `om:*` keys (jitter applied per-write) |
| `WORKFLOW_SERVICE_BASE_URL` / `WORKFLOW_TIMEOUT_MS` | — / `3000` | §8.8 delegate-impact gate — fail-closed |
| `REALM_PROVISIONER_BASE_URL` / `REALM_PROVISIONER_TIMEOUT_MS` | — / `3000` | Invite/MFA-reset fail-closed; realm-config/session-revoke fail-open |
| `CATALOG_ADMIN_BASE_URL` / `CATALOG_ADMIN_TIMEOUT_MS` | — / `3000` | **NOT fail-open** — surfaces `catalog_unavailable` (503) on a cold-cache-plus-failure intersection |
| `GROUP_MAPPING_BASE_URL` / `GROUP_MAPPING_TIMEOUT_MS` | — / `300` | I-10 JIT resolution — fails open (never fails a SAML login) |
| `DELEGATION_BASE_URL` / `DELEGATION_TIMEOUT_MS` | — / `300` | §8.8.4 dept-scope pre-filter — fails open to tenant-wide scoping |
| `INVITATION_EXPIRY_DAYS` | `7` | Must equal Keycloak's invite action-token lifespan |
| `SEAT_OVERAGE_GRACE_DAYS` | `30` | Informational seat-overage grace window (SEAT-5) — Billing owns enforcement |
| `SUBSCRIPTION_GRACE_DAYS` | `30` | I-16's cancellation-to-suspension threshold — this service owns the math so RP's sweep never keeps a second copy of the rule |
| `MAX_LIFECYCLE_EVENT_SKEW_SECONDS` | `300` | EVT-15 future-time clamp |
| `INVITE_REINVITE_COOLDOWN_MINUTES` / `INVITE_MAX_PER_TENANT_PER_HOUR` | `15` / `60` | PI-11/PI-12 invite throttles |
| `DOCS_ENABLED` / `DOCS_AUTH_TOKEN` | `false` / — | Swagger UI gating outside dev |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (unset, opt-in) | OTLP/gRPC collector |

---

## Security

| Topic | Guidance |
|---|---|
| **Three-layer isolation** | RLS (`FORCE ROW LEVEL SECURITY` + `REVOKE ALL FROM PUBLIC` + `tenant_isolation` policy on `app.tenant_id`) is the DB-level floor; the service never trusts a body-supplied `tenant_id`/`user_id`/role; the gateway strips client-supplied identity headers before mesh entry |
| **RLS-6** | `app.tenant_id` is set transaction-locally (`SET LOCAL`) on every checkout so a pooled PgBouncer connection can never leak a GUC value to the next client — CI greps for any non-`LOCAL` `SET` as a forbidden pattern |
| **Cross-tenant admin access** | Requires the `BYPASSRLS` role `org_membership_migrator` — never the app role — used only by reconciler jobs, business-metric exporters, and I-16's subscription-lapse read |
| **AUTH-8 privilege reduction** | A suspend/removal/de-privilege commits its own state change first (authoritative immediately), then makes a best-effort, fail-open `RevokeUserSessions` call to Realm Provisioner; the guaranteed cutoff falls back to a TTL bound (access-token lifetime + 300 s cache TTL) if that call fails |
| **AUTH-7 operator defense-in-depth** | `platform_operator` is re-checked at the handler layer (`RequireOperatorRole`) before any DB access, independent of network-layer isolation |
| **No Keycloak credential of any kind** | This service never calls the Keycloak Admin API — every mutation against Keycloak goes through Realm Provisioner's own sole seam |
| **PII boundary** | `pending_invitations` (email + full name until acceptance) is the one deliberate PII exception; GDPR erasure-by-email is the sole non-`user_id`-keyed path |

---

## Observability

**SLOs.** I-8 is the platform's tightest read: **p50 ≤ 15 ms** (cache hit) / **p99 ≤ 30 ms** (cache miss, cold Postgres read).

### Metrics

Per the IAM Platform Observability Standard's three-tier hierarchy (`internal/adapter/outbound/metrics/business.go`), with `domain`/`service`/`environment` injected centrally in `metrics.Register(environment)` — never left to a call site:

- **Tier 1 — `platform_*`** (concept common across domains; carries `domain="iam"` + `service` + `environment`): `platform_messages_received_total{queue}` / `platform_messages_processed_total{queue}` / `platform_messages_failed_total{queue}` (SQS consumer lifecycle, both queues), `platform_duplicate_messages_total{consumer}` (IDEMP-4), `platform_dlq_messages_total{event_type,reason}` (EVT-15 clamp), `platform_dependency_request_seconds{target_service,endpoint}` / `platform_dependency_errors_total{target_service,endpoint,outcome}` (`target_service` = catalog\|group_mapping\|delegation, the downstream peer — distinct from the `service` const label).
- **Tier 2 — `iam_*`** (concept shared across IAM-domain services; carries `service` + `environment`, no `domain`): `iam_rls_violations_total{violation_type}`, `iam_auth_session_revoke_failed_total{reason}`, `iam_lifecycle_event_skipped_total{event_type}` (EVT-14), `iam_lifecycle_event_lag_seconds{event_type}`.
- **Tier 3 — `iam_org_membership_*`** (unique to this service): `unknown_event_acknowledged_total`, `delegate_suspend_impact_total`, `tenant_ownerless_escalated_total`, `seat_overage_started_total`, `seat_limit_reached_total`, `invite_throttled_total{reason}`, `realm_sync_failed_total`, `delegate_removal_blocked_total`, `delegate_reassignment_total`, `membership_exists_check_total` (I-15), plus 4 gauges refreshed by ticker goroutines every 5 minutes against the BYPASSRLS sys pool: `tenant_ownerless`, `realm_sync_pending`, `seat_overage_active`, `pending_invitations_stale`.

A CI script (`.github/scripts/check-metric-naming.sh`) enforces naming/suffix/label-set rules on every PR. Passthrough `http_*`/`events_*`/`outbox_*`/`sqs_*` (`platform-gincommon`/`platform-events`) and `pgcommon_*` pool/query instruments round out the surface.

### Tracing and logs

`gincommon.InitTracingFromEnv()`, gated on `OTEL_EXPORTER_OTLP_ENDPOINT`, plus a `db.query` span per query via `pgcommon.Config.Tracer`. Structured Zap logs never carry a credential, password seed, or secret — only identifiers (`tenant_id`, `user_id`, `event_type`, `idempotency`-adjacent keys where relevant).

---

## Deployment

### Container image — two binaries

| Binary | Path in image | Purpose |
|---|---|---|
| `iam-org-membership` | `/iam-org-membership` | HTTP server (`cmd/server`) — the image's `ENTRYPOINT`. Serves all three route prefixes, runs the outbox runner + SQS consumer + 4 metric-exporter goroutines |
| `reconciler` | `/reconciler` | One-shot reconciler (`cmd/reconciler`), dispatched via `--job=<name>` by the 7 K8s CronJobs |

Two-stage `Dockerfile`: `golang:1.26.6-alpine` builder (base image pinned to a SHA digest), runtime is `gcr.io/distroless/static-debian12:nonroot` (no shell, non-root, UID 65532) — only the two compiled binaries are copied in. `EXPOSE 8080 9090`.

### Helm chart

`deploy/helm/` renders one `Deployment` plus the 7 `CronJob`s above from the same image reference. HPA: `minReplicas: 2` / `maxReplicas: 8`, CPU 70% / memory 75%. PDB `minAvailable: 1`. Resources: CPU `100m`/`500m`, Memory `256Mi`/`512Mi`. `terminationGracePeriodSeconds: 75` (30 s HTTP drain + 30 s outbox drain + 15 s buffer). `startupProbe` covers the migration window on cold start (120 s budget) before liveness/readiness take over.

### Migration safety

Since this service has never been deployed, the schema is one consolidated `000000_initial_schema` migration (`internal/adapter/outbound/postgres/migrations/`) — the incremental decomposition history (ADR-0007/ADR-0008) was squashed rather than carried forward as dead schema history.

---

## CI

Nine workflow files:

- **`ci.yml`** — orchestrator. Runs `validate-test.yml` and `validate-quality.yml` in parallel with `build-image` (Hadolint → Buildx cached build → Trivy CVE scan → smoke tests). On push to `main`: builds+pushes to GHCR with provenance+SBOM.
- **`validate-test.yml`** (reusable) — `make test-ci` (unit + postgres/RLS + integration, `-race`, merged coverage) → coverage threshold gate (**95%**) → `go-arch-lint` → Swagger staleness check → event-schema sync check (`schema-gov extract --check`).
- **`validate-quality.yml`** (reusable) — `go mod verify` → `gofmt` check → `go mod tidy` drift check → `go vet` → `golangci-lint` → `govulncheck`.
- **`changelog-check.yml`** — fails a PR touching `internal/`, `api/`, `deploy/`, or `cmd/` without a `CHANGELOG.md` update.
- **`release.yml`** — tag-triggered release pipeline: re-validate → build+cross-compile → Docker build/push/sign → optional deploy-gate → GitHub Release publish.
- **`schema-registry.yml`** — registers this service's event schemas to the shared Glue registries: PR read-only validate+diff, push-to-`main` full validate→diff→register→changelog.
- **`schema-prune.yml`** — monthly dry-run orphan-schema report plus an operator execute path.
- **`schema-health-quarterly.yml`** — read-only quarterly lifecycle-annotation lint + Glue version-accumulation scan.
- **`freeze-watchdog.yml`** — daily cron alerting when a schema-registry `SCHEMA_FREEZE` has been left active too long.

**Required GitHub Actions repository secret: `GO_PRIVATE_TOKEN`** — every workflow that runs `go mod download` needs it to fetch the private `platform-events`/`platform-gincommon`/`platform-pgcommon` modules.

---

## Docker

### What the bundled `docker-compose.yml` starts

| Container | Image | Host port(s) | Purpose |
|---|---|---|---|
| `app` | built from local `Dockerfile` | `8080`, `9090` | This service itself, when run via `docker compose up` rather than `make run` |
| `postgres` | `postgres:17-alpine` | `5534 → 5432` | Primary store |
| `pgbouncer` | `edoburu/pgbouncer:latest` | `5533 → 5432` | Transaction-pooling proxy in front of `postgres` |
| `redis` | `valkey/valkey:8-alpine` | `6380 → 6379` | Advisory cache |
| `floci` | `floci/floci:2.1.0-compat` | `4567 → 4566` | SNS/SQS/Glue Schema Registry (all services free/always-on) |

`make docker-up` only starts `postgres`/`pgbouncer`/`redis`/`floci` — not `app` — so local dev typically still runs the service via `make run` for fast rebuilds. Host ports are deliberately offset from sibling `iam-user-profile2`'s (5433/5434/6379/4566) so both stacks can run side-by-side.

### Building the service image

```bash
docker build -t iam-org-membership:local --secret id=go_private_token,src=<(echo "$GO_PRIVATE_TOKEN") .
```

### Health and readiness

| Endpoint | Returns | Checks |
|---|---|---|
| `GET /healthz` | `200 {"status":"ok"}` | Pure liveness — never inspects a dependency |
| `GET /readyz` | `200`/`503` | App Postgres pool, sys (BYPASSRLS) Postgres pool, Valkey, and the outbox runner, each via its own `Health(ctx)` |

### Minimum required environment variables

```bash
# PostgreSQL
PG_HOST=localhost
PG_USER=org_membership_app
PG_PASSWORD=<password>
PG_DBNAME=org_membership
MIGRATION_DATABASE_URL=postgres://org_membership_app:...@localhost:5534/org_membership?sslmode=disable
SYSTEM_DATABASE_URL=postgres://org_membership_migrator:...@localhost:5534/org_membership?sslmode=disable

# Events (outside dev)
SNS_TOPIC_MEMBERSHIP_ARN=arn:aws:sns:ap-south-1:...:iam-membership-events
SNS_TOPIC_TENANT_ARN=arn:aws:sns:ap-south-1:...:iam-tenant-events
GLUE_REGISTRY_MEMBERSHIP_NAME=iam-membership-events
GLUE_REGISTRY_TENANT_NAME=iam-tenant-events

# Outbound service clients
WORKFLOW_SERVICE_BASE_URL=http://localhost:8082
REALM_PROVISIONER_BASE_URL=http://localhost:8083
CATALOG_ADMIN_BASE_URL=http://localhost:8084
```

---

## Cross-service dependencies

Reads (I-8 hot path, list endpoints, I-15) have **no** synchronous cross-service dependency — Postgres + Valkey only.

| Operation | Sync dependency | Posture | On failure |
|---|---|---|---|
| Invite (P-6) | Realm Provisioner | fail-closed | `503 realm_provisioner_unavailable`, no invite written |
| MFA reset (P-34) | Realm Provisioner (RP-9) | fail-closed | `503`, no `MFAReset` emitted — no reconciler exists for this |
| User removal / dept demotion·removal (P-8/I-5/P-10/P-11) | Workflow | fail-closed | `503 workflow_service_unavailable`, no change |
| — dept-scope precision leg | Delegation | degrade | Falls back to tenant-wide impact scoping (correct, less precise) |
| Suspension advisory (P-7) | Workflow | fail-open | Suspend commits; advisory omitted |
| `local_accounts_enabled` change (P-2) | Realm Provisioner | fail-open + durable reconcile | Commits; `realm_sync_pending`, reconciler converges |
| Any P-6/P-24/P-10 catalog validation | Catalog Service | **fail-closed** | `503 catalog_unavailable` — the one dependency here that is deliberately not fail-open |
| I-10 SAML JIT group resolution | Group Mapping Service | fail-open | Empty resolution — never fails a login |
| RP-C3 subscription-lapse sweep (I-16) | — (this service is the callee) | — | A Realm Provisioner outage just means its own sweep sees a stale/empty list this cycle |

---

## Out of scope

| Concern | Where it lives |
|---|---|
| Credentials, password policy, MFA enforcement, JWT issuance | Keycloak |
| Display identity, signature, OOO presentation flag | User Profile |
| Global department catalog / plan entitlement catalog | Catalog / Admin Config Service |
| Group→dept/role JIT mapping tables | Group Mapping / JIT Config Service |
| Tender ACL overlay `tender_acl_entries` | Tender ACL Service |
| Delegation record/lifecycle/policy/events, OOO coordination | Delegation Service |
| Realm/Keycloak Admin API mutations | Realm Provisioner |
| Workflow template authoring, `assignee_overrides` state | Workflow Service |
| Audit records | Audit Log Service |
| Metered resource consumption / quota enforcement | Usage & Metering |
| Pricing / currency | Billing |

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, extending the service, testing requirements, and the PR checklist.

| Document | Description |
|---|---|
| [`.claude/CLAUDE.md`](.claude/CLAUDE.md) | Top-level guidance for Claude Code working in this repo |
| [`.claude/architecture.md`](.claude/architecture.md) | Clean Architecture directory tree, shared library integration, dep rules |
| [`.claude/database-schema.md`](.claude/database-schema.md) | 8 tables, enums, RLS/tenant/seat/migration/trigger invariants |
| [`.claude/api-caching-events.md`](.claude/api-caching-events.md) | Full endpoint catalogue, cache TTLs, event catalogue, status-code table |
| [`.claude/request-flows.md`](.claude/request-flows.md) | Provisioning, delegate-impact resolution, invite→accept, concurrency, GDPR |
| [`.claude/operations.md`](.claude/operations.md) | Security, observability, configuration, CI/CD, dependency degradation matrix |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | Detailed architecture narrative with diagrams |
| [`docs/lld/iam-lld-org-membership-service.md`](docs/lld/iam-lld-org-membership-service.md) | Full LLD v2.3 — §16 open-question register, §17 error taxonomy, §19 migration strategy |
| [`docs/runbook-schema-registry.md`](docs/runbook-schema-registry.md) | Schema-governance operator runbook |

---

## License / ownership

BCBP Solutions FZC LLC — internal platform service. Not for external distribution.
