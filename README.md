# iam-org-membership

The Org & Membership (Core) Service — the **organizational/business system of record** in the IAM subsystem. It owns tenants (plan, subscription lifecycle, trial/cancellation/suspension timers, MFA freshness), department activation, tenant memberships and elevated tenant-level roles, department-level role grants, the two-step invite→accept staging flow, and per-tenant feature-flag overrides. It is the hot-path source of truth AuthZ Enrichment reads on every authenticated request via I-8.

**Repository:** `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership`
**Module:** Go 1.26.10 · private module · deployed as three binaries from one image (`cmd/server`, `cmd/reconciler`, `cmd/migrate`)
**Design:** Refines the IAM HLD (`IAM HLD v1.41`, §5.6); LLD **v2.38** (`docs/lld/iam-lld-org-membership-service.md`) — where LLD and HLD disagree, HLD is authoritative. This repo is post-decomposition: departments/plans catalog ownership moved to Catalog / Admin Config Service, SAML group→dept/role mapping moved to Group Mapping / JIT Config Service, tender ACL overlays moved to Tender ACL Service, and delegation grants + OOO coordination moved to Delegation Service (ADR-0007 + ADR-0008) — this repo is what's left after all four extractions.

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

**Core's only remaining delegation touchpoint:** a synchronous delegate-impact gate on user removal (via `port.WorkflowClient` — talks to the *Workflow* Service, unaffected by the decomposition) plus a synchronous department-scope precision lookup on the admin dept-membership Assign/Remove path via `port.DelegationCheckClient`. On a Delegation Service outage, a removal (P-11) degrades to tenant-wide impact scoping (still correct, less precise) and a demotion (P-10) proceeds ungated — never a hard failure.

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

**41 active endpoints** across four route prefixes (`/api/v1/*` — including the caller-self `/api/v1/me/*` — `/api/v1/internal/*`, `/api/v1/operator/*`, `/api/v1/public/*`). Source of truth: `internal/adapter/inbound/http/router.go`; the generated REST contract is `docs/swagger/swagger.yaml` (`make swag`), not hand-authored. Retired route IDs (below) are fully unregistered — not `410 Gone` — and never reused.

**Middleware chain:** a 1 MB body cap, then `gincommon.ObservabilityMiddlewares` (`PanicRecovery → RequestID → Tracing → CorrelationHeaders → Metrics → Logging → Timeout`) and `NormalizeAuthErrors` (adds this service's `code` — `missing_identity_headers`/`ambiguous_identity_headers`/`invalid_request_context` — to gincommon's own 401/400 bodies) on every route; the protected `/api/v1` group adds `gincommon.ProtectedMiddlewares` (`RequireAuth → ContextMiddleware`) → `GUCBridgeMiddleware` (binds `app.tenant_id` transaction-locally, RLS-6) → `RequireJSONContentType`. The service trusts gateway-injected headers (`x-user-id`, `x-tenant-id`, `x-tenant-roles`) under mesh mTLS — no JWT parsing.

### Public routes (22) — `/api/v1/*`

| # | Method & Path | Purpose |
|---|---|---|
| P-1 | `GET /tenants/:id` | Tenant details (name, plan, locale, `mfa_freshness_seconds`) |
| P-2 | `PATCH /tenants/:id` | Update name, locale, `local_accounts_enabled`, `mfa_freshness_seconds` |
| P-3 | `GET /tenants/:id/departments` | List active departments for tenant |
| P-4 | `GET /tenants/:id/members` | List tenant members, cursor-paginated. Roles and dept memberships hydrated in two batch queries (`WHERE user_id = ANY($2)`) — 3 total queries per page regardless of page size (was N+1 before rev 2.20) |
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
| P-35 | `GET /me/session-context` | The signed-in user's own tenant context for the frontend: `tenant_id`, `roles`, `departments` [`{id, level}`], `plan`, `feature_flags`, `read_only` (the values AuthZ Enrichment injects as headers), plus `mfa_freshness_seconds`, `locale`. No parameters — the caller is `x-tenant-id`/`x-user-id`. Reads the I-8 projection + `om:tenant` caches; `404` with no membership, `403` for a suspended member |

*Retired, never reused:* P-14/15/16/17/29 (group-mapping admin CRUD → Group Mapping Service), P-18/19/20/32/33 (delegation → Delegation Service), P-21/22/23 (tender ACL → Tender ACL Service).

### Internal routes (17) — `/api/v1/internal/*`, mesh-mTLS + NetworkPolicy only

| # | Method & Path | Caller | Purpose |
|---|---|---|---|
| I-1 | `POST /tenants` | Realm Provisioner / Signup BFF | Provision a new tenant row. **Requires Catalog Service** (plan lookup + dept seeding) — returns `503 catalog_unavailable` on cold-cache + Catalog outage. bff-signup retries I-1 up to `maxI1InlineRetries=2` inline, then falls back to `202 tenant_pending`; the backstop also retries on redelivery. |
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
| I-17 | `GET /tenants/:id/plan-context` | Entitlements Service | Tenant plan context — `{plan, status, licensed_seats, feature_flags, record_version}` from `om:tenant` cache (CD-13) |
| I-18 | `GET /tenants/:id/plan-change-check?target_plan=` | Billing Service | Pre-flight check for a plan change — `{allowed, blockers[]}`, current blocker: `sso_configured` (CD-20) |
| I-19 | `GET /tenants/:id/members/:user_id/eligibility?department_id=&required_level=` | Workflow Services | Assignee eligibility — `{eligible, reason, role_level}`; active member, department active, level ≥ required (preparator < reviewer < approver); uncached |
| CR-4 | `POST /trial-eligibility` | Signup BFF | Pre-signup email-hash gate — checks whether the HMAC-SHA256 hex of the owner's lower-cased email has already been used for a trial (`trial_signup_ledger`). `200` eligible; `409 TRIAL_ALREADY_USED` already used. No path tenant (pre-tenant). |

*Retired, never reused:* I-6/I-7 (quotas → Usage & Metering), I-12 (tender-ACL check → Tender ACL Service).

### Operator routes (2) — `/api/v1/operator/*`, `platform_operator` only

| # | Method & Path | Purpose |
|---|---|---|
| O-4 | `PATCH /operator/tenants/:id/feature-flags` | Full-replacement of a tenant's feature-flag override delta |
| O-7 | `POST /operator/tenants/:id/reassign-owner` | Recover an ownerless tenant; grants `tenant_owner` to an existing active member |

*Retired, never reused:* O-1/2/3/5/6 (department/plan catalog admin → Catalog Service).

### Public, unauthenticated routes (1) — `/api/v1/public/*`

| # | Method & Path | Purpose |
|---|---|---|
| PUB-1 | `GET /public/tenants/resolve?identifier=` | Pre-login tenant/realm resolution for the identifier-first login flow (IAM HLD §8.2.7). There's no auth and no gateway headers, and it bypasses `ext_authz`. Resolves a company slug or a pending invitation's email (an active member's email isn't resolvable, LLD §16 OQ-11). It's always `200` (`{found:false}` on no match, never `404`, PUBRES-2), runs on the BYPASSRLS `sysPool`, and is uncached. |

The only route in this service with no authentication. It's registered directly on the gin engine by `registerPublicRoutes`, outside the protected `/api/v1` group, so it never runs `RequireAuth`, `GUCBridgeMiddleware` or any role gate. At the edge, `ingress.publicResolve.enabled` (HTTPRoute mode, off by default) renders an Exact, GET-only HTTPRoute for this path with a non-merging SecurityPolicy (no gateway JWT/`ext_authz` on it) and a per-client-IP rate limit (20/hour) — `deploy/helm/templates/publicresolve.yaml`, LLD §10.2. CAPTCHA escalation (PUBRES-3) belongs in the WAF, not Envoy; it and the `tenant_id`-hint disclosure review are still open (LLD §16 OQ-10).

---

## Input validation

Domain-rule failures are raised as a `*domain.DomainError` wrapping one of the sentinel error values in `internal/core/domain/errors.go`; `HandleError` (`internal/adapter/inbound/http/middleware.go`) maps each sentinel to an RFC 9457 `application/problem+json` response via `platform-gincommon v2`. The `errorCode` field carries the domain-specific UPPER_SNAKE code (e.g. `"TENANT_NOT_FOUND"`, `"SEAT_LIMIT_REACHED"`) — never the platform type's generic default. Domain errors that carry structured context (`active_workflows`, `licensed_seats`, `record_version`, `retry_after_seconds`) include those as RFC 9457 §3.1 extension members alongside the standard envelope. A raw `*pgconn.PgError` that escapes the repository layer untranslated is classified with platform-pgcommon's SQLSTATE helpers — `IsConnectionException`/`IsInsufficientResources`/`IsOperatorIntervention`/`IsSystemError`/`IsPoolClosed` → `503 DB_UNAVAILABLE`, `IsQueryCanceled`/`IsLockNotAvailable` → `503 REQUEST_TIMEOUT`, `IsDeadlock`/`IsSerializationFailure` → `409 CONFLICT`; anything else falls back to `500 INTERNAL_ERROR`.

```json
{
  "type": "https://errors.company.com/platform/conflict",
  "title": "Conflict",
  "status": 409,
  "detail": "Seat limit reached — upgrade your plan or remove a member.",
  "errorCode": "SEAT_LIMIT_REACHED",
  "traceId": "4bf92f3577b34da6a3ce929d0e0e4736",
  "timestamp": "2026-10-18T10:45:22Z",
  "licensed_seats": 10,
  "active_users": 9,
  "pending_invitations": 1
}
```

`DomainError.Details` are sent as RFC 9457 §3.2 extension members alongside the standard envelope fields (`record_version`, `active_workflows`, `workflow_ids`, `allowed_actions`, `licensed_seats`, `retry_after_seconds`, ...) — not in a nested `details` object. They go through platform-gincommon's extension support (`problem.Error.WithExtensions`), so the library serialises them: a detail can never overwrite a standard member (`status`, `errorCode`, `traceId`, …), and an internal-error 500 carries none.

### Notable validation rules

| Rule | Enforcement |
|---|---|
| `tenant_id` in the request body/path is never trusted for authorization | Identity comes exclusively from gateway-injected headers; RLS returns zero rows on any mismatch (API-1/API-2) |
| A tenant-role reconcile (P-28) can never assign `member` | `chk_tr_no_member` CHECK, backstopped by service-layer validation (`400 invalid_role`) — `member` is derived at read time, never stored (TR-7) |
| A `tenant_owner`-affecting mutation can never leave zero active owners | `422 last_owner_removal` (TM-8), serialized via `SELECT ... FOR UPDATE` on the `tenants` row (TM-13) so two concurrent removals can't both pass |
| Seat cap is enforced transactionally, not just pre-flight | `SELECT ... FOR UPDATE` on `tenants` under the same tx that counts `active + pending` against `licensed_seats` (SEAT-1) |
| Optimistic locking | 7 of 10 tables carry `record_version` (`touch_row` DB trigger); a version mismatch returns `409 optimistic_lock_conflict` with the current version in the body |
| Invitation rate limiting | Per-email cooldown (`INVITE_REINVITE_COOLDOWN_MINUTES`, `429 reinvite_too_soon`) and per-tenant / per-actor hourly ceilings (`INVITE_MAX_PER_TENANT_PER_HOUR` / `INVITE_MAX_PER_ACTOR_PER_HOUR`, `429 invite_rate_limited`) — all checked pre-flight, before any Realm Provisioner call |

---

## Architecture

Clean Architecture — dependencies point inward; outer layers never import inner layers. Full layer diagrams, package dependency graph, and shared-library integration are in **[`.claude/architecture.md`](.claude/architecture.md)**; standalone Mermaid diagrams live in **[`docs/architecture/`](docs/architecture/README.md)**.

```
iam-org-membership/
├── cmd/
│   ├── server/                        # HTTP composition root: pool+GUC wiring, migrate-at-startup unless RUN_MIGRATIONS=false, outbox runner, 3 SQS consumers (tenant-orgm-q/billing-orgm-q/catalog-orgm-q), 6 metric-exporter goroutines (exporters.go — 4 gauges + iam_rls_violations_total counter); wiring.go holds the topic-publisher/codec/SQS-consumer builder helpers factored out of main.go
│   ├── reconciler/                    # Single binary, --job=<name>; jobs/ holds the 7 CronJob entry points
│   └── migrate/                       # One-shot schema migration (postgres.MigrateSchema: platform-events outbox schema, then domain migrations) — the Helm pre-install/pre-upgrade hook Job
├── internal/
│   ├── core/
│   │   ├── domain/                    # Entities, value objects, DomainError catalogue — no external deps
│   │   ├── port/                      # TenantRepository, MembershipRepository, AuthZRepository, Inbox (processed_events dedup), ReconcilerStore, WorkflowClient, RealmProvisionerClient, CatalogAdminClient, GroupMappingClient, DelegationCheckClient, TokenServiceClient (AUTH-9/IB-3)
│   │   └── service/                   # MembershipService, DeptMembershipService, DepartmentService, TenantService, AuthZService, ProvisioningService, OperatorService, InvitationService, RoleLabelService, CatalogService, GroupMappingService, SubscriptionLapseService, TenantResolutionService (PUB-1)
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                  # Gin handlers (P-*/I-*/O-*), DTOs, middleware, router.go, Swagger UI
│       │   ├── consumer/              # 3 SQS consumers: tenant-orgm-q, billing-orgm-q (EVT-14/15/16 guards), catalog-orgm-q (DepartmentCatalogChanged / PlanCatalogChanged — cache invalidation, CACHE-11); dedup.go (processOnce/ackUnknown over port.Inbox)
│       │   └── sqspipeline/           # Inbound pipeline every queue runs: NewConsumer/EnvForQueue (consumer.go), RequireDLQ + routeRejectsToDLQ (dlq.go), consumed-schema Validator + Handler (inbound_schema.go)
│       └── outbound/
│           ├── postgres/              # Repository impls (incl. InboxRepository, ReconcilerStore, GaugeRepository, TrialSignupLedgerRepository), MigrateSchema + migrations/ (000000_initial_schema, 000001_check_constraints, 000002_pub1_pending_invitations_email_idx, 000003_cd37_residency_mode, 000004_cd50_pending_tenant_events, 000005_cr4_trial_signup_ledger)
│           ├── valkey/                # Cache adapter (go-redis/v9) — advisory only
│           ├── eventbus/              # RoutingPublisher (two topics) + outbox runner + embedded event schemas
│           ├── workflow/               # Workflow Service HTTP client (§8.8 delegate-impact)
│           ├── realmprovisioner/       # Realm Provisioner HTTP client — CreateInvitedUser/DeleteUser/PatchRealmConfig/RevokeUserSessions/ResetMFA
│           ├── catalogadmin/           # Catalog Service HTTP client — departments/plans (NOT fail-open)
│           ├── groupmappingclient/     # Group Mapping Service HTTP client — I-10 JIT resolution (fails open)
│           ├── delegationcheck/        # Delegation Service HTTP client — §8.8.4 dept-scope precision lookup (fails open)
│           ├── tokenservice/           # Token Service HTTP client — AUTH-9/IB-3 service-account-not-grantable check (fails open)
│           ├── httpx/                  # Shared otelhttp-instrumented http.Client factory — httpx.NewClient(timeout) is built by all 6 outbound clients above; otelhttp.NewTransport handles traceparent injection + client-span emission, and the transport forwards x-request-id
│           └── metrics/                # registry.go (Tier-1/Tier-2 shared collectors from the Platform Observability Registry) + business.go (Tier-3 iam_org_membership_* collectors)
├── pkg/requestctx/                    # Typed RequestContext{UserID, TenantID, Roles, ClientIP, UserAgent}
├── api/
│   └── asyncapi.yaml                  # AsyncAPI — iam.membership.events (12) + iam.tenant.events (2)
├── docs/
│   ├── lld/                           # LLD v2.20 — §16 open-question register, §17 error taxonomy
│   ├── swagger/                       # Generated REST contract (make swag) — not hand-authored
│   ├── architecture/                  # Standalone Mermaid diagrams
│   ├── observability/                 # Observability standard implementation, generated metric-registry.md, migration.md, runbooks.md
│   └── runbook-*.md                   # schema-registry, outbox-dead-letter, i8-latency, rls-cross-tenant operator runbooks
├── deploy/                            # Helm chart (incl. migrate-job.yaml hook), monitoring alerts/SLO rules/dashboards, IAM policy
├── scripts/                           # init-floci.sh, init-db.sql, merge_coverage.py, swagger patcher
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
| `inbound_pipeline` (`internal/adapter/inbound/sqspipeline`) | `adapters_inbound`, `port`, `domain`, `observability` |
| `adapters_outbound` (postgres/valkey/eventbus/workflow/realmprovisioner/catalogadmin/groupmappingclient/delegationcheck/tokenservice) | `port`, `domain`, `eventschema`, `observability`, `httptransport` |
| `httptransport` (`internal/adapter/outbound/httpx` — all 6 outbound HTTP clients build via `httpx.NewClient(timeout)`) | `requestctx` only (for the `x-request-id` it forwards) |
| `reconciler_jobs` (`cmd/reconciler/jobs`) | `domain`, `port`, `observability`, `requestctx` |
| `cmd` (server/reconciler/migrate) | Everything above |

Plus **RLS-6**, CI-enforced by grep: every write to `app.tenant_id` must be `SET LOCAL` (transaction-scoped), never a session-scoped `SET`, so a value can never leak across a pooled PgBouncer connection.

### Storage and messaging

| Concern | Technology | Notes |
|---|---|---|
| **Primary store** | PostgreSQL 17, database `org_membership` on shared RDS (Multi-AZ, PgBouncer transaction pooling) | 10 tables, `FORCE ROW LEVEL SECURITY` on all 8 tenant-scoped tables; GUC `app.tenant_id` bound transaction-locally |
| **Cache** | Valkey (Redis-compatible) via `go-redis/v9` | Advisory-only (CACHE-2/9) — a miss or outage falls through to Postgres, never a hard failure |
| **Events (outbound)** | AWS SNS + transactional outbox (`platform-events`) | Two topics via `RoutingPublisher`: `iam.membership.events` (12 event types) and `iam.tenant.events` (2 — `TenantCreated`/`TrialStarted`) |
| **Events (inbound)** | AWS SQS, 3 queues | `tenant-orgm-q`, `billing-orgm-q`, `catalog-orgm-q` (cache invalidation only), each with its own `-dlq`; Glue header stripped by `events.GlueDecodeCodec`, payload validated against the embedded consumed schema, permanent rejects sent straight to the DLQ; EVT-14 recency guard + EVT-15 future-time clamp + EVT-16 tenant-state relay |
| **Schema registry** | AWS Glue, one registry per topic (SCHEMA-7) | `iam-membership-events`, `iam-tenant-events`; governed by `platform-schemagov`. Version UUIDs resolved once at startup by definition (`GetSchemaByDefinition`) — no per-event Glue call, no refresher |

### Shared library dependencies

| Library | Version | Purpose |
|---|---|---|
| `platform-gincommon` | v1.6.0 | HTTP middleware, Zap logging, OTel tracing, Prometheus metrics, the Platform Observability Registry and `metricslint` |
| `platform-events/v2` | v2.2.1 | Transactional outbox (ordered enqueue), SNS publisher, SQS consumer with DLQ forwarding, inbox (`processed_events` dedup), Glue decode codec |
| `platform-pgcommon/v2` | v2.0.1 | pgx/v5 pool, RLS GUC injection, per-transaction statement/lock timeouts, migrations, SQLSTATE error helpers |

---

## Integrating with other services

### 1. Prerequisites

Every caller must run on the internal service mesh (mTLS + NetworkPolicy) for `/api/v1/internal/*`, or forward gateway-validated `x-user-id`/`x-tenant-id`/`x-tenant-roles` headers for `/api/v1/*`. There is no JWT parsing in this service — the gateway/mesh has already done it.

**Tenant on internal routes (IAPI-6).** On `/api/v1/internal/tenants/:id/*` the tenant in the **path** is authoritative. Callers must still send `x-tenant-id`, and should send the same tenant as the path: a differing value is overridden, logged (`tenant_header_path_mismatch`) and counted (`iam_org_membership_tenant_header_path_mismatch_total`), and once `STRICT_TENANT_PATH_VALIDATION` is enabled it is rejected with `400 TENANT_MISMATCH`. I-8 uses its `?tenant_id=`; I-1 and I-16 use `x-tenant-id`.

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

**Wire format:** when a Glue registry is configured, `data` is a base64 string of `[0x03][0x00][16-byte schema version UUID]` + the JSON payload, and `dataschema` holds that UUID — wire a decoder (`events.WithConsumerCodec`; stripping the 18-byte header suffices, no registry lookup needed). The UUID is the version matching the publishing build's embedded schema, so it changes only when a build with a new schema is deployed.

**Idempotency:** record the envelope `id` (UUID v7) against your own consumer name before committing any side effect — delivery is at-least-once. **Ordering:** events with a subject are enqueued with `outbox.EnqueueOrdered` keyed `<tenant_id>/<subject>` (under a transaction-scoped advisory lock on that key), so one user's events are *published* in commit order — but SNS/SQS delivery is not ordered end to end, so keep your own recency guard; this service's own consumer uses an `EVT-14` recency guard keyed on `tenants.last_event_at` under a row lock.

### 8. Handling errors

Every non-2xx response is the flat envelope shown above. See `docs/lld/iam-lld-org-membership-service.md` §17 for the complete error taxonomy.

### 9. Rate limits

No generic per-caller/per-endpoint rate limiter exists in `internal/adapter/inbound/http/` — inbound throttling, if any, is the gateway's concern. The only request-level throttling in this codebase is invite-specific business logic: PI-11 (per-email cooldown) and PI-12 (per-tenant hourly ceiling), both pre-flight-checked before P-6 ever calls Realm Provisioner.

### 10. Background reconcilers you may observe

9 CronJobs, one Helm manifest, each `cmd/reconciler --job=<name>` against the same image:

| CronJob (`--job=`) | Schedule | Purpose |
|---|---|---|
| `invitation-expiry` | `*/5 * * * *` | Sweeps `pending_invitations` past `expires_at` → `expired`, sets `kc_cleanup_pending` |
| `invitation-kc-cleanup` | `*/10 * * * *` | Idempotent `RealmProvisioner.DeleteUser` for rows with `kc_cleanup_pending` |
| `realm-config-sync` | `*/10 * * * *` | Retries `realm_sync_pending` tenants against Realm Provisioner (T-15 Option A) |
| `seat-overage-reconcile` | `0 */6 * * *` | Self-heals `overage_since`/`grace_ends_at` for any missed set/clear |
| `trial-cleanup` | `0 2 * * *` | Hard-deletes trial tenants past grace beyond `trial_expired` |
| `outbox-prune` | `0 3 * * *` | Prunes published `outbox_events` past `OUTBOX_RETENTION_DAYS` |
| `processed-events-prune` | `0 4 * * *` | Prunes `processed_events` rows past `PROCESSED_EVENTS_TTL_DAYS` |
| `parked-events-replay` | `*/5 * * * *` | EVT-18 backstop: replays events parked for a tenant before its row existed, once the row exists (I-1 normally replays them immediately) |
| `trial-ledger-prune` | `30 4 * * *` | CR-4 retention: deletes `trial_signup_ledger` rows older than `TRIAL_LEDGER_RETENTION_DAYS` (default 730), in batches, so the email HMAC is not kept forever (GDPR storage limitation); a pruned email can start a new trial |

The 4 business-metric gauges (`iam_org_membership_tenant_ownerless`, `iam_org_membership_realm_sync_pending`, `iam_org_membership_seat_overage_active`, `iam_org_membership_pending_invitations_stale`), plus a 5th exporter for the `iam_rls_violations_total` counter (`pgadapter.GaugeRepository.RLSViolationCounts`, mirrors `iam-user-profile`'s `runRLSViolationExporter`), run as **ticker goroutines inside `cmd/server`** (`runBusinessExporters`, `exporters.go`), not CronJobs.

---

## Local development

### Prerequisites

- Go 1.26.10+
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
| `make setup` | Copy `.env-example` → `.env` (if absent) and install the `.githooks/pre-commit` hook |
| `make install-hooks` | (Re)install the pre-commit hook only |
| `make tidy` / `make fmt` / `make fmt-check` / `make vet` | Go basics; `fmt-check` mirrors CI, does not modify files |
| `make lint` | `golangci-lint` via `go tool` — default build plus every test build tag (`integration`, `e2e`, `helm`) |
| `make arch-lint` | `go-arch-lint` against `.go-arch-lint.yml` (the same script CI runs) |
| `make metrics-inventory` | Regenerate `docs/observability/metric-registry.md` from the pinned registry and a real scrape |
| `make metrics-lint` | platform-gincommon's `metricslint` on a real `/metrics` scrape plus the alert/dashboard references (the same script CI runs) |
| `make helm-access-check` | CD-19 access matrix: renders the chart with the staging and prod values and asserts, by parsing the YAML, which routes each cross-namespace caller (Billing, Entitlements, Signup BFF, `workflow-app`) can reach in the Istio AuthorizationPolicy and that `billing`/`commercial`/`bff` are admitted only per pod in the NetworkPolicy; also pins PUB-1's edge resources (`ingress.publicResolve`: Exact GET route, non-merging SecurityPolicy, per-IP rate limit) (`test/helm/`, needs `helm`; CI runs it after `helm-check.sh`) |
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
| `make run` | Run the server locally (sources `.env`, kills port 8080 first; migrates at startup since `RUN_MIGRATIONS` is unset) |
| `make mock-servers` | Local mock Workflow (:8082) / Realm Provisioner (:8083) / Catalog Admin (:8084) for manual testing (`MOCK_ARGS="-workflow-active 2"` makes P-8 return 409) |
| `make build` | Compile all three binaries to `bin/` (`iam-org-membership`, `reconciler`, `migrate`) and check `./internal/...`/`./pkg/...` compile |
| `make cover` / `make cover-func` | Coverage HTML report / per-function summary |
| `make ci` | `tidy` + `fmt-check` + `vet` + `lint` + `arch-lint` + `test-ci` + `build` |
| `make docker-up` / `make docker-down` | Start/stop PostgreSQL + PgBouncer + Valkey + floci |
| `make extract-schemas` / `make schema-validate` / `make schema-diff` / `make schema-register` / `make schema-prune` | `platform-schemagov:0.4` image workflows: extract from `api/asyncapi.yaml`, validate, diff two schema versions, register to both Glue registries, dry-run orphan scan |
| `make schema-verify` | Pre-deploy check: every produced schema's definition is registered and `AVAILABLE` in its Glue registry — the same lookup the pod runs at startup |
| `make swag` / `make swag-check` | Regenerate / verify freshness of the Swagger REST contract |
| `make docs-serve` | `make run`, then serve the docs surface (`/`, `/swagger`, `/asyncapi`) |
| `make pin-base-images` | Re-pin the Dockerfile base-image SHA digests (and `.docker-digests`) |
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

An outbox insert is atomic with the business write — a `2xx` response guarantees an `outbox_events` row exists. The payload is validated against its embedded schema before the insert (`ValidatingCodec`) and stored as plain JSON; the Glue header is added only at SNS publish time.

Inbound, each of this service's own queues runs: envelope parse → `events.GlueDecodeCodec` (strip the Glue header) → DLQ router → `platform_messages_*` counters → consumed-schema validation → consumer `Handle`. A payload that fails its consumed schema, or an EVT-15 future-dated event, is forwarded to the queue's `-dlq` immediately through platform-events' own `DLQPublisher.SendToDLQ` (original body and attributes, not a re-encoded copy) and acked instead of being retried; the reject reason is counted on `iam_org_membership_consumed_event_rejected_total{reason=schema_violation|future_time_clamp}`. A body that can't be decoded at all is forwarded by the library itself (`events.WithDLQForwarding`) rather than dropped. Transient errors retry; at the 4th receive (`SQS_MAX_RECEIVE_COUNT`, one below the RedrivePolicy's 5) the library forwards the message to the DLQ.

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

### Step 2b — Verify event delivery in the browser (floci-ui)

`make docker-up` also starts **floci-ui**, a web console for floci, at **http://localhost:4500**. It's a faster way to confirm an event landed on the right queue than shelling into the CLI each time:

1. Open **http://localhost:4500** → sidebar → **Integration → SQS**. You'll see all 20 queues `init-floci.sh` provisioned (`membership-audit-q`, `membership-workflow-q`, `tenant-orgm-q`, ...), each with a **Messages** column.
2. Trigger an event — either exercise a real endpoint (`make run` + a P-* call, per Step 3 below) or publish one directly to skip the app entirely:
   ```bash
   docker compose exec floci aws --region ap-south-1 sns publish \
     --topic-arn arn:aws:sns:ap-south-1:000000000000:iam-membership-events \
     --message '{"id":"demo-1","type":"TenderAssigneeOverridden","tenant_id":"t1"}' \
     --message-attributes 'EventType={DataType=String,StringValue=TenderAssigneeOverridden}'
   ```
3. Refresh the SQS list. The **Messages** count should have gone to `1` on exactly the queues whose filter policy (§7.3.2) matches that `EventType` — for `TenderAssigneeOverridden` that's `membership-audit-q` (catch-all) and `membership-workflow-q`, and nowhere else. That's the visual equivalent of `get-queue-attributes --attribute-names ApproximateNumberOfMessages` — a wrong or missing count on a queue you expected to receive the event means the filter policy or the event's `EventType` attribute is wrong.

Two things the UI does **not** do (yet):
- **Read a message's payload.** The UI shows queue metadata (message counts, ARN, retention) only, no message browser — see Step 2c below for the CLI equivalent. Tracked upstream: [floci-io/floci-ui#148](https://github.com/floci-io/floci-ui/pull/148) ("add SQS queue explorer" — peek/receive, delete-message, purge), open but not yet merged/released as of 2026-09-15. Once it ships we can bump the pinned `floci/floci-ui` tag in `docker-compose.yml` and this becomes click-through.
- **Browse SNS topics/subscriptions.** floci-ui has no SNS adapter yet, so the topic → queue fan-out wiring itself (subscriptions, filter policies) isn't visible there — see Step 2d below.

### Step 2c — Retrieve the event body (CLI, until floci-ui#148 ships)

Once you see a non-zero **Messages** count on a queue in the UI, get the actual event JSON out of it:

```bash
# Peek without deleting — the message stays and becomes visible again after
# the queue's VisibilityTimeout (30s by default).
docker compose exec floci aws --region ap-south-1 sqs receive-message \
  --queue-url http://floci:4566/000000000000/membership-workflow-q \
  --max-number-of-messages 10 --message-attribute-names All
```

Every subscription `init-floci.sh` creates sets `RawMessageDelivery=true` (LLD §7.3.2), so `Body` is already the plain event JSON — no SNS envelope to unwrap. Pretty-print it with `jq`:

```bash
docker compose exec floci aws --region ap-south-1 sqs receive-message \
  --queue-url http://floci:4566/000000000000/membership-workflow-q \
  --max-number-of-messages 10 --message-attribute-names All \
  | jq -r '.Messages[] | .Body | fromjson'
```

Or skip SQS entirely and read the outbox table directly — fastest during dev, and shows the plain-JSON payload before any Glue/Noop wire-format encoding is applied at publish time:

```bash
docker compose exec postgres psql -U org_membership_app -d org_membership -c \
  "SELECT event_type, jsonb_pretty(payload::jsonb) FROM outbox_events ORDER BY created_at DESC LIMIT 3;"
```

Swap `membership-workflow-q` for whichever queue you're inspecting; see `test.md`'s "Reading events from SQS" section for the full cookbook (count messages, delete-and-consume, filter by `EventType`, purge).

### Step 2d — Inspect SNS topics and subscriptions (CLI, no floci-ui equivalent)

floci-ui has no SNS adapter (see Step 2b above), so the topic → queue fan-out wiring itself — which queues are subscribed to which topic, and their filter policies (LLD §7.3.2) — is CLI-only:

```bash
# All 10 subscriptions across both topics
docker compose exec floci aws --region ap-south-1 sns list-subscriptions

# Just iam-membership-events' 6 fan-out queues
docker compose exec floci aws --region ap-south-1 sns list-subscriptions-by-topic \
  --topic-arn arn:aws:sns:ap-south-1:000000000000:iam-membership-events \
  --query 'Subscriptions[].{Queue:Endpoint,Arn:SubscriptionArn}' --output table
```

To see *why* a queue did or didn't receive an event — its filter policy — grab a `SubscriptionArn` from the above and inspect it:

```bash
docker compose exec floci aws --region ap-south-1 sns get-subscription-attributes \
  --subscription-arn <SubscriptionArn> \
  --query 'Attributes.{FilterPolicy:FilterPolicy,RawMessageDelivery:RawMessageDelivery}'
```

A missing `FilterPolicy` means the subscription is a catch-all (e.g. `membership-audit-q`, `tenant-audit-q`) — it receives every event on that topic. A queue you expected an event on but that showed `Messages: 0` in floci-ui (Step 2b) almost always means the event's `EventType` message attribute isn't in this list.

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
| `outbox_events` row never gets `published_at` set | Topic ARN mismatch, or outbox runner not started | Re-check `.env`'s `SNS_TOPIC_*_ARN` against `aws --region ap-south-1 sns list-topics` (or floci-ui at http://localhost:4500) |
| `outbox_events` empty after a write | Row was published and pruned, or the write never committed | Re-check the HTTP response code — a `2xx` guarantees the row was committed |
| Messages keep reappearing after `receive-message` | Normal — SQS visibility timeout, not deletion | Use `delete-message` |
| `make run` panics with `resolve glue schema ... by definition` | This build's schema definition isn't registered in the Glue registry (e.g. a schema changed since floci started) | `make docker-down && make docker-up` to re-run `init-floci.sh`, or unset `GLUE_REGISTRY_*_NAME` for plain JSON |
| An inbound event lands in `*-orgm-q-dlq` immediately | Permanent reject — check its `DLQReason` attribute (`schema_violation` = payload fails the embedded consumed schema; `future_time_clamp` = EVT-15) | Fix the payload or `api/asyncapi.yaml`; see `docs/runbook-schema-registry.md` |

---

## Testing

### Canonical tests (do not break)

- **`test/postgres/rls_test.go`** — the canonical RLS cases, including **Case 5**: no cross-tenant GUC leak across a pooled PgBouncer connection. CI additionally greps for any non-`LOCAL` `SET app.tenant_id` as a forbidden pattern.
- **`test/postgres/subscription_lapse_test.go`** — I-16's cross-tenant, BYPASSRLS-bound query returns only tenants past grace and drops a tenant off the next poll once suspended (self-idempotence).
- **`test/integration/pipeline_dlq_test.go`** — runs the real `sqspipeline` against floci: a poison pill is forwarded to the DLQ on its first delivery; a transient failure is forwarded at receive count 4 (`SQS_MAX_RECEIVE_COUNT`).
- **`test/unit/authz_service_test.go`** — I-8's business-logic composition (TR-7 derived-role injection, PLAN-6 feature-flag merge, graceful degradation on a Catalog Service outage), unit-tested with no Docker at all now that `AuthZService` depends on `port.AuthZRepository` instead of `*pgcommon.Pool` directly.

### Coverage

`.github/scripts/coverage-gate.sh` reads `go tool cover -func=coverage.out`'s total and fails below `COVERAGE_THRESHOLD` (default **95%**, not overridden in this repo's `validate-test.yml`). Coverage is measured over `./internal/...` and `./pkg/...` (`COVER_PKG_LIST` in the Makefile), merged across the unit/postgres/integration suites via `scripts/merge_coverage.py` (max-count strategy). The last `make test-ci` run reported a merged total of **97.4%** (2026-10-03), above the enforced floor; the earlier 99.1% figure (2026-09-20) predates this branch's observability and inbound-pipeline changes. Residual uncovered statements are mostly defensive branches (e.g. a `json.Marshal` error path on an always-marshalable struct) that are impractical to exercise without contriving unrealistic inputs.

---

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `APP_NAME` / `APP_ENV` / `APP_PORT` | `iam-org-membership` / `dev` / `8080` | Service identity (`APP_NAME` is the one source of the service name on metrics and spans — `OTEL_SERVICE_NAME` is not read), env-gated behavior + the metrics `environment` label (`local\|dev\|test\|staging\|prod` only — anything else fails startup), HTTP listen port |
| `OBSERVABILITY_DOMAIN` | `iam` | Observability Standard `domain` const label (also set in code via `gincommon.Config.Domain`) |
| `BUILD_VERSION` | ldflags `buildVersion` | Overrides the build version on `platform_build_info` and traces; Helm sets it to the image tag |
| `METRICS_PORT` | `9090` | Dedicated `/metrics` listener, separate from `APP_PORT` |
| `DATABASE_URL` or `PG_HOST`/`PG_PORT`/`PG_USER`/`PG_PASSWORD`/`PG_DBNAME`/`PG_SSLMODE` | — | App pool DSN — required one way or the other |
| `PG_MAX_CONNS` / `PG_MIN_CONNS` / `PG_SLOW_QUERY_THRESHOLD` | `20` / `0` / `200ms` | Pool sizing + slow-query log threshold |
| `PG_BOUNCER_MODE` | `true` (dev) | Transaction-scoped GUC injection when behind PgBouncer |
| `MIGRATION_DATABASE_URL` | app DSN | Direct (non-PgBouncer) DSN — migrations acquire a session-scoped advisory lock, so set it whenever the app DSN goes through PgBouncer. The only DSN `cmd/migrate` reads (falls back to `DATABASE_URL`/`PG_*`) |
| `PG_STATEMENT_TIMEOUT` / `PG_LOCK_TIMEOUT` | `5s` / `2s` (Helm) | Applied per transaction by pgcommon (`Config.StatementTimeout` / `LockTimeout`) on both pools; never on migrations. Reconciler jobs use `RECONCILER_STATEMENT_TIMEOUT` (`120s`) instead |
| `RUN_MIGRATIONS` | `true` | `false` in Helm: the migrate hook Job (`cmd/migrate`) owns the schema; unset or true, the server migrates at startup (local dev, tests) |
| `MIGRATE_TIMEOUT` | `10m` | `cmd/migrate` only — bounds the whole run, lock wait included (Helm `migrations.job.timeout`, below the Job's `activeDeadlineSeconds: 900`) |
| `RECONCILER_JOB` / `RECONCILER_TIMEOUT` / `RECONCILER_BATCH_LIMIT` | — / `5m` / `500` | `cmd/reconciler` only — job name when `--job` is not passed / whole-run deadline / per-run batch bound |
| `TRIAL_GRACE_DAYS` | `15` | `trial-cleanup` only — days past `trial_expired` before a trial tenant is hard-deleted |
| `SQS_MAX_RECEIVE_COUNT` / `SQS_DRAIN_TIMEOUT` / `SQS_HANDLER_TIMEOUT` / `SQS_QUEUE_DEPTH_INTERVAL` | `4` / `15s` / `20s` / `60s` | DLQ forward threshold (below the RedrivePolicy's 5) / consumer drain on shutdown / bound on one handler call / `platform_queue_depth`·`platform_dlq_depth` sampling |
| `PROMETHEUS_PUSHGATEWAY_URL` | — | Where each reconciler run pushes its metrics; empty skips the push |
| `SYSTEM_DATABASE_URL` | — | BYPASSRLS pool for reconciler jobs, business-metric exporters, and I-16's cross-tenant read — must target `org_membership_migrator` in production |
| `VALKEY_URL` | `localhost:6380` (dev) | Advisory cache; `rediss://` required outside dev |
| `SNS_TOPIC_MEMBERSHIP_ARN` / `SNS_TOPIC_TENANT_ARN` | — | **Required outside dev.** The two `RoutingPublisher` topic ARNs |
| `SQS_TENANT_ORGM_QUEUE_URL` / `SQS_BILLING_ORGM_QUEUE_URL` | — | Inbound lifecycle-event queues |
| `SQS_CATALOG_ORGM_QUEUE_URL` | required in staging/prod; set in `.env-example` | Inbound `catalog-orgm-q` ← `DepartmentCatalogChanged` / `PlanCatalogChanged` (LLD rev 2.39). The queue and its SNS subscription are SRE/platform Terraform |
| `SQS_TENANT_ORGM_CONCURRENCY` / `SQS_BILLING_ORGM_CONCURRENCY` / `SQS_CATALOG_ORGM_CONCURRENCY` | `4` / `2` / `2` | Per-queue consumer concurrency overlay onto the shared `platform-events` `eventcfg.LoadSQS()` env contract |
| `GLUE_REGISTRY_MEMBERSHIP_NAME` / `GLUE_REGISTRY_TENANT_NAME` | — | Unset → `NoopCodec` (plain JSON) |
| `AWS_REGION` / `AWS_ENDPOINT_URL` / `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | `ap-south-1` / — | floci override in dev; IRSA in production |
| `OUTBOX_POLL_INTERVAL` / `OUTBOX_BATCH_SIZE` / `OUTBOX_MAX_ATTEMPTS` / `OUTBOX_DRAIN_TIMEOUT` / `OUTBOX_PUBLISH_CONCURRENCY` / `OUTBOX_PUBLISH_TIMEOUT` / `OUTBOX_STARTUP_JITTER` / `OUTBOX_CLAIM_LEASE_DURATION` | `500ms` / `50` / `5` / `30s` / `4` / `10s` / `2s` / `10m` | Outbox runner tunables |
| `PROCESSED_EVENTS_TTL_DAYS` | `8` | Consumer dedup retention — deliberately > the 7-day SQS message lifetime (PE-1) |
| `OUTBOX_RETENTION_DAYS` | `8` | `outbox-prune` CronJob retention, now consumed by `platform-events`' own `outbox.Runner.PrunePublished` (migrated off a local hand-rolled batched delete) |
| `CACHE_TTL_SECONDS` | `300` | Base TTL for `om:*` keys (jitter applied per-write) |
| `WORKFLOW_SERVICE_BASE_URL` / `WORKFLOW_TIMEOUT_MS` | — / `3000` | §8.8 delegate-impact gate — fail-closed |
| `REALM_PROVISIONER_BASE_URL` / `REALM_PROVISIONER_TIMEOUT_MS` | — / `3000` | Invite/MFA-reset fail-closed; realm-config/session-revoke fail-open |
| `CATALOG_ADMIN_BASE_URL` / `CATALOG_ADMIN_TIMEOUT_MS` | — / `3000` | **NOT fail-open** — surfaces `catalog_unavailable` (503) on a cold-cache-plus-failure intersection |
| `GROUP_MAPPING_BASE_URL` / `GROUP_MAPPING_TIMEOUT_MS` | — / `300` | I-10 JIT resolution — fails open (never fails a SAML login) |
| `DELEGATION_BASE_URL` / `DELEGATION_TIMEOUT_MS` | — / `1000` | §8.8.4 dept-scope pre-filter — fails open to tenant-wide scoping (raised from 300ms, Gap-8 fix: too tight under load/cold-start) |
| `TOKEN_SERVICE_BASE_URL` / `TOKEN_SERVICE_TIMEOUT_MS` | — / `1000` | AUTH-9/IB-3 service-account-not-grantable check (P-6/I-3 invite/register, P-10/P-28 role-grant) — fails open, degrading to allow (structural composite-FK bar, TR-8/DM-4, is the primary guarantee) |
| `AUDIT_LOG_BASE_URL` / `AUDIT_LOG_TIMEOUT_MS` | — / `3000` | iam-audit-log AL-5 direct-write for `config.tenant_setting.changed` (P-2), `invitation.created/revoked/expired` (P-6/P-31/reconciler). **Fail-open** — calls are best-effort fire-and-forget; an outage never blocks P-2 or invite flows. Unset logs a WARN at startup and skips all AL-5 writes silently. Sustained failures surface on `IAMAuditDeliveryFailed` alert (`iam_org_membership_audit_delivery_failed_total{entry_type}`) |
| `INVITATION_EXPIRY_DAYS` | `7` | Must equal Keycloak's invite action-token lifespan |
| `SEAT_OVERAGE_GRACE_DAYS` | `30` | Informational seat-overage grace window (SEAT-5) — Billing owns enforcement |
| `SUBSCRIPTION_GRACE_DAYS` | `30` | I-16's cancellation-to-suspension threshold — this service owns the math so RP's sweep never keeps a second copy of the rule |
| `MAX_LIFECYCLE_EVENT_SKEW_SECONDS` | `300` | EVT-15 future-time clamp |
| `INVITE_REINVITE_COOLDOWN_MINUTES` / `INVITE_MAX_PER_TENANT_PER_HOUR` / `INVITE_MAX_PER_ACTOR_PER_HOUR` | `15` / `60` / `10` | PI-11/PI-12 invite throttles (per email / per tenant / per inviting actor, SECURITY-5) |
| `DOCS_ENABLED` / `DOCS_AUTH_TOKEN` | `false` / — | Swagger UI gating outside dev |
| `STRICT_TENANT_PATH_VALIDATION` | `false` | Helm `iam.strictTenantPathValidation`. `true` rejects an `/internal/tenants/:id/*` request whose `x-tenant-id` differs from the path tenant (`400 TENANT_MISMATCH`) instead of overriding it |
| `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_EXPORTER_OTLP_INSECURE` | collector (Helm) / `localhost:4317` (dev) | OTLP/gRPC trace export, read by platform-gincommon. An empty value is treated as unset and falls back to `localhost:4317` — it does not disable export |
| `OTEL_TRACES_SAMPLER_RATIO` | `0.1` (Helm) / `1.0` (dev) | Head sampling ratio in [0,1] |
| `OTEL_PROPAGATOR_BAGGAGE` / `OTEL_TRACES_IGNORE_REMOTE_PARENT_SAMPLED` | `false` / `false` | W3C baggage propagation; whether a sampled remote parent is re-sampled (true only on edge services) |
| `LOG_LEVEL` / `LOG_SAMPLING` | `info` (Helm) / `debug` (dev); `false` | platform-gincommon logger level and zap sampling, all three binaries (the migrate Job gets them too) |

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

- **Tier 1 — `platform_*`** (concept common across domains; carries `domain="iam"` + `service` + `environment`): `platform_messages_received_total` / `platform_messages_processed_total` / `platform_messages_failed_total` (`{queue,event_type[,reason]}`, SQS consumer lifecycle — recorded by platform-events), `platform_duplicate_messages_total{queue,event_type}` (IDEMP-4, counted by platform-events' inbox), `platform_dlq_messages_total{operation,event_type,reason}` (counted only by platform-events, once per DLQ forward; this service's own reject reasons — `future_time_clamp` (EVT-15) and `schema_violation` — are on the Tier-3 `iam_org_membership_consumed_event_rejected_total`, and both page), `platform_dependency_request_seconds{dependency,operation,outcome}` (`dependency` = the downstream peer, e.g. catalog\|group_mapping\|delegation — distinct from the `service` const label). Tier-1 collectors are built from the central Platform Observability Registry (`gincommon.NewShared*Vec`). Identity: `gincommon.Config.Domain = "iam"` (also `OBSERVABILITY_DOMAIN`), environment from `APP_ENV` (`local|dev|test|staging|prod` only). Label values follow the registry vocabulary, and CI runs platform-gincommon's `metricslint` on a real scrape (`make metrics-lint`).
- **Tier 2 — `iam_*`** (concept shared across IAM-domain services; carries `service` + `environment`, no `domain`): `iam_rls_violations_total{violation_type}`, `iam_auth_session_revoke_failed_total{reason}`, `iam_lifecycle_event_skipped_total{event_type}` (EVT-14), `iam_lifecycle_event_lag_seconds{event_type}`.
- **Tier 3 — `iam_org_membership_*`** (unique to this service; built with `gincommon.NewCounterVec`/`NewGauge`/`NewHistogramVec`, which validate the name and derive the prefix from the service name — no raw `prometheus.New*`): `unknown_event_acknowledged_total{event_type}`, `consumed_event_rejected_total{event_type,reason}`, `processed_events_duplicates_total{consumer}`, `dependency_call_duration_seconds` / `dependency_call_failures_total{target_service,endpoint[,reason]}`, `delegate_suspend_impact_total`, `delegate_removal_blocked_total{scope}`, `delegate_reassignment_total{action}`, `tenant_ownerless_escalated_total{reason}`, `seat_overage_started_total{cause}`, `seat_limit_reached_total{plan}`, `invite_throttled_total{reason}`, `realm_sync_failed_total{stage}`, `membership_exists_check_total{caller,result}` (I-15), `public_tenant_resolve_total{result}` (PUB-1), `catalog_cache_invalidated_total` (catalog-orgm-q Gap-12), `audit_delivery_failed_total{entry_type}` (AL-5 best-effort failure counter — fires `IAMAuditDeliveryFailed` alert when nonzero for 10 min), `trial_eligibility_check_total{result}` (CR-4 Signup BFF gate, `eligible`\|`already_used`), plus 4 gauges refreshed by ticker goroutines every 5 minutes against the BYPASSRLS sys pool: `tenant_ownerless`, `realm_sync_pending`, `seat_overage_active`, `pending_invitations_stale`.

The full generated inventory is [`docs/observability/metric-registry.md`](docs/observability/metric-registry.md) (`make metrics-inventory`; commit it with any metric change). `/metrics` is served by `gincommon.MetricsHandler()` on `METRICS_PORT`. A CI script (`.github/scripts/check-metric-naming.sh`) enforces naming/suffix/label-set rules on every PR (no raw `prometheus.New*`; shared collectors use registry names only, service collectors Tier-3 names only), and `make metrics-lint` (`.github/scripts/metricslint.sh`) runs gincommon's `metricslint` on a real scrape, the alert/dashboard references and inventory drift. Passthrough `http_*`/`events_*`/`outbox_*`/`sqs_*` (`platform-gincommon`/`platform-events`) and `pgcommon_*` pool/query instruments round out the surface.

### Alerts and dashboards

Alert rules live in `deploy/monitoring/app-alerts.yml` (26), `slo-rules.yml` (5), `schema-registry-alerts.yml` (4) and the Helm `deploy/helm/templates/prometheusrule.yaml` (23). Alerts built on a still-`proposed` registry metric carry `metric_status: proposed` — among them `IAMOrgMembershipOutboxOrderingBlocked`, `OutboxBacklog`, `DLQBacklog`, `DBPoolSaturated`, `DBAcquireErrors` and `DBRetriesExhausted`. Dashboards (`deploy/monitoring/dashboards/`): `business-health`, `data-integrity`, `i8-latency`. Per-alert runbooks: [`docs/observability/runbooks.md`](docs/observability/runbooks.md); renamed/removed metric names: [`docs/observability/migration.md`](docs/observability/migration.md).

### Tracing and logs

`gincommon.InitTracingWithConfig` (service name, build version, `domain="iam"` and the resolved environment passed explicitly, so traces and metrics carry the same identity), exporting to `OTEL_EXPORTER_OTLP_ENDPOINT` (sampler, baggage and log settings in the config table above), plus a `db.query` span per query via `pgcommon.Config.Tracer`. Structured Zap logs never carry a credential, password seed, or secret — only identifiers (`tenant_id`, `user_id`, `event_type`, `idempotency`-adjacent keys where relevant).

---

## Deployment

### Container image — three binaries

| Binary | Path in image | Purpose |
|---|---|---|
| `iam-org-membership` | `/iam-org-membership` | HTTP server (`cmd/server`) — the image's `ENTRYPOINT`. Serves all four route prefixes, runs the outbox runner + 3 SQS consumers + 6 metric-exporter goroutines (5 gauges + `iam_rls_violations_total` counter) |
| `reconciler` | `/reconciler` | One-shot reconciler (`cmd/reconciler`), dispatched via `--job=<name>` by the 7 K8s CronJobs; pushes each run's metrics to `PROMETHEUS_PUSHGATEWAY_URL`, grouped by `cronjob` |
| `migrate` | `/migrate` | One-shot schema migration (`cmd/migrate` → `postgres.MigrateSchema`: platform-events' `outbox.ApplySchema`, then the domain migrations); exits 0 on success or "already up to date", 1 on failure, with operator hints for a dirty migration or a version missing from the build |

Three-stage `Dockerfile`, every base image pinned to a SHA digest: a `golang:1.26.10-alpine` builder; a `debian:bookworm-slim` `certs` stage, whose patched CA bundle (ca-certificates deb12u2, DLA-4845-1) replaces distroless's until distroless ships it; and the `gcr.io/distroless/static-debian12:nonroot` runtime (no shell, non-root, UID 65532), into which only the three compiled binaries and that CA bundle are copied. `EXPOSE 8080 9090`.

### Helm chart

`deploy/helm/` renders one `Deployment` plus the 7 `CronJob`s above from the same image reference. HPA: `minReplicas: 2` / `maxReplicas: 8`, CPU 70% / memory 75%. PDB `minAvailable: 1`. Resources: CPU `100m`/`500m`, Memory `256Mi`/`512Mi`. `terminationGracePeriodSeconds: 75` (max(30 s HTTP drain, 15 s SQS drain, in parallel) + 30 s outbox drain + 10 s pool drain + 5 s buffer). Schema migrations run once per release in the `<release>-migrate` pre-install/pre-upgrade hook Job (`deploy/helm/templates/migrate-job.yaml`, hook weight 0 — the chart's Secret is a weight -10 hook so `MIGRATION_DATABASE_URL` exists first; `backoffLimit: 1`, `activeDeadlineSeconds: 900`). A failed migration fails the release instead of crash-looping pods. The migrate pod deliberately does not carry the chart's selector labels, so the Service, PDB and NetworkPolicy never select it. Replicas start with `RUN_MIGRATIONS=false`; `migrations.job.enabled=false` falls back to migrate-at-startup. `startupProbe` covers cold start (120 s budget) before liveness/readiness take over.

### Migration safety

Since this service has never been deployed, the decomposition history (ADR-0007/ADR-0008) was squashed into one consolidated `000000_initial_schema` migration rather than carried forward as dead schema history. Later changes are additive migrations on top of it (`internal/adapter/outbound/postgres/migrations/`): `000001_check_constraints` (length CHECKs), `000002_pub1_pending_invitations_email_idx` (PUB-1 email lookup index), `000003_cd37_residency_mode` (`tenants.residency_mode`), `000004_cd50_pending_tenant_events` (CD-50 parked-events table), and `000005_cr4_trial_signup_ledger` (CR-4 cross-tenant `trial_signup_ledger` table for the Signup BFF trial-eligibility gate). platform-events' own outbox schema (`outbox_events`, `outbox_dead_letters`) is applied by `outbox.ApplySchema` before them.

---

## CI

Nine workflow files:

- **`ci.yml`** — orchestrator. Runs `validate-test.yml` and `validate-quality.yml` in parallel with `build-image` (Hadolint → Buildx cached build → Trivy CVE scan → smoke tests). On push to `main`: builds+pushes to GHCR with provenance+SBOM.
- **`validate-test.yml`** (reusable) — `make test-ci` (unit + postgres/RLS + integration, `-race`, merged coverage) → coverage threshold gate (**95%**) → `make test-e2e` (real HTTP router via `httptest`, no coverage/`-race`) → `go-arch-lint` → Swagger staleness check → event-schema sync check (`schema-gov extract --check`, with schema-gov v0.4.0 installed from source; the known `mfareset.json` drift is the one tolerated exception).
- **`validate-quality.yml`** (reusable) — `go mod verify` → HTML-escaped-operator check on workflow files → RLS-6 non-`LOCAL` `SET app.tenant_id` grep → metric naming/registry-ratification checks → observability-standard lint (`metricslint.sh`, same as `make metrics-lint`) → events-bypass check (`check-forbidden-events-bypass.sh`, no raw SNS/SQS SDK calls or hand-built `events.Envelope{}` outside `platform-events`) → outbox-access check (`check-outbox-access.sh`, no hand-rolled SQL against `outbox_events` outside `platform-events`) → `gofmt` check → `go mod tidy` drift check → `go vet` → `golangci-lint` (incl. a `forbidigo` rule banning raw `pgxpool`/`pgx.Connect`/`sql.Open` outside `pgcommon.NewPool`) → `govulncheck` → Dockerfile base-image digest check (against `.docker-digests`) → `helm-check.sh` → `make helm-access-check` (CD-19 access matrix).
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
| `floci-ui` | `floci/floci-ui:0.5.0` | `4500 → 4500` | Web console for `floci` — http://localhost:4500 |

`make docker-up` only starts `postgres`/`pgbouncer`/`redis`/`floci`/`floci-ui` — not `app` — so local dev typically still runs the service via `make run` for fast rebuilds. Host ports are deliberately offset from sibling `iam-user-profile2`'s (5433/5434/6379/4566) so both stacks can run side-by-side.

### Building the service image

```bash
docker build -t iam-org-membership:local --secret id=go_private_token,src=<(echo "$GO_PRIVATE_TOKEN") .
```

### Health and readiness

| Endpoint | Returns | Checks |
|---|---|---|
| `GET /healthz` | `200 {"status":"ok"}` | Pure liveness — never inspects a dependency |
| `GET /readyz` | `200`/`503` | App Postgres pool, sys (BYPASSRLS) Postgres pool, Valkey, outbox runner, and SQS consumer goroutines (OBS-1), each via its own `Health(ctx)` |

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

### 1. Synchronous outbound calls (this service → other services)

Reads (I-8 hot path, list endpoints, I-15) have **no** synchronous cross-service dependency — Postgres + Valkey only.

| Operation | Dependency | Adapter package | Posture | On failure |
|---|---|---|---|---|
| Invite (P-6) | Realm Provisioner | `outbound/realmprovisioner/` | fail-closed | `503 realm_provisioner_unavailable`, no invite written |
| MFA reset (P-34) | Realm Provisioner (RP-9) | `outbound/realmprovisioner/` | fail-closed | `503`, no `MFAReset` emitted — no reconciler exists for this |
| Invitation KC cleanup reconciler | Realm Provisioner | `outbound/realmprovisioner/` | fail-open | Retry next cron cycle; `kc_cleanup_pending` stays set |
| `local_accounts_enabled` change (P-2) | Realm Provisioner | `outbound/realmprovisioner/` | fail-open + durable reconcile | Commits; `realm_sync_pending`, reconciler converges |
| Session revoke on suspend/remove (AUTH-8) | Realm Provisioner | `outbound/realmprovisioner/` | fail-open (best-effort) | Cutoff falls back to access-token TTL + 300 s cache TTL |
| User removal / dept demotion·removal (P-8/I-5/P-10/P-11) | Workflow Service | `outbound/workflow/` | fail-closed | `503 workflow_service_unavailable`, no change committed |
| Suspension advisory (P-7) | Workflow Service | `outbound/workflow/` | fail-open | Suspend commits; advisory call omitted |
| I-1 plan lookup + dept seeding (ProvisioningService.TrialSignup) | Catalog / Admin Config Service | `outbound/catalogadmin/` | **fail-closed** | `503 catalog_unavailable` on cold-cache + failure — calls happen BEFORE the Postgres tx opens (§8.1). bff-signup retries handle this correctly. |
| Any P-6/P-24/P-10 catalog validation | Catalog / Admin Config Service | `outbound/catalogadmin/` | **fail-closed** | `503 catalog_unavailable` — the one dependency deliberately not fail-open |
| I-10 SAML JIT group resolution | Group Mapping / JIT Config Service | `outbound/groupmappingclient/` | fail-open | Empty resolution — never fails a login |
| Dept-scope precision leg of delegate-impact pre-check (§8.8.4) | Delegation Service | `outbound/delegationcheck/` | degrade | Falls back to tenant-wide impact scoping (correct, less precise) |
| Service-account-not-grantable check (Invite P-6/I-3, role-grant P-10/P-28, AUTH-9/IB-3) | Token Service | `outbound/tokenservice/` | fail-open | Degrades to allowing the operation — the structural composite-FK bar (TR-8/DM-4) is the primary guarantee |
| RP-C3 subscription-lapse sweep (I-16) | — (this service is the callee) | — | — | A Realm Provisioner outage just means its own sweep sees a stale/empty list this cycle |

### 2. Inbound callers (other services → this service)

| Caller | Endpoints used | Purpose |
|---|---|---|
| **Realm Provisioner** | I-1, I-2, I-16 | Provision tenant row; set `realm_id`/`realm_type`/`keycloak_shard`; poll subscription lapses (RP-C3) |
| **Signup BFF** | CR-4, I-1 | Pre-signup trial-eligibility gate (CR-4, step E — email-hash check before RP-1); provision new tenant row on trial sign-up (I-1, includes `email_hash` for the atomic ledger write) |
| **AuthZ Enrichment** | I-8, I-14 | Hot-path full membership projection (p50 ≤ 15 ms); authoritative MFA-freshness read for step-up gate |
| **Billing** | I-11 (same handler as P-27), I-18 | Seat-usage pre-check before a seat-reduction plan change; SSO-downgrade guard before committing a plan change (CD-20) |
| **Entitlements Service** | I-17 | Tenant plan context (`plan`, `status`, `licensed_seats`, `feature_flags`) for re-deriving effective entitlement set after billing/tenant events (CD-13) |
| **LLM Ingestion Service** | I-9 | Tenant default locale for prompt assembly |
| **Workflow Service** | I-13, I-19 | Validate-and-emit on tender assignee override — persists nothing here; assignee eligibility read (default assignees at publish, overrides, reassignment, delegation reroute) |
| **Tender ACL Service** | I-15 | Grant-time membership-existence check (replaces composite FK lost across DB split) |
| **Delegation Service** | I-15 | Grant-time membership-existence check (replaces composite FK lost across DB split) |

### 3. Async event dependencies

**Events this service consumes:**

| Queue | Producer | Events |
|---|---|---|
| `tenant-orgm-q` | Realm Provisioner | `TenantIdentityProvisioned` (signup provisioning backstop: creates the tenant via I-1 if missing), `TenantRealmReady`, `TenantConverted`, `DirectPaidSignup`, `TrialExpired`, `TrialReactivated`, `TenantSuspended`, `TenantOffboarded`, `TenantReactivated` |
| `billing-orgm-q` | Billing | `TenantSeatsChanged`, `TenantPlanChanged`, `TenantPaymentPastDue`, `TenantSubscriptionCancelled`, `TenantReactivated` |
| `catalog-orgm-q` | Catalog / Admin Config Service | `DepartmentCatalogChanged` (clears `om:departments`), `PlanCatalogChanged` (clears `om:plans`, retires the plan's cached I-8 entries via `om:plans:gen:{plan}`; never writes `feature_flags`) |

**Events this service produces** (via `iam.membership.events` / `iam.tenant.events`) and their downstream consumers:

| Consumer | Queue | Events consumed |
|---|---|---|
| **Audit Log Service** | `membership-audit-q` | Catch-all — all 12 membership events + `TenantCreated`, `TrialStarted` on `iam.tenant.events` |
| **AuthZ Enrichment** | `membership-authz-q` | `DepartmentMembership*`, `TenantRole*`, `MembershipRevoked` — cache invalidation for `om:memberships:{tenant}:{user}` |
| **Realm Provisioner** | `membership-realm-q` | `DepartmentMembershipGranted`/`LevelChanged`, `TenantRoleGranted`/`Revoked` — add/drop `requires-mfa` realm role |
| **Notification** | `membership-notification-q` | `DepartmentMembership*`, `TenantRole*`, `TenantSeatOverage*` — user/admin emails + seat-overage banner |
| **Workflow Service** | `membership-workflow-q` | `TenderAssigneeOverridden`, `DepartmentMembership*`, `TenantStateChanged`, `MembershipRevoked`, `TenantMembershipsPurged` — assignee reconciliation + tenant-state pause/resume/terminate + removed-user cleanup + tenant purge |
| **Billing** | `membership-billing-q` | `TenantSeatOverageStarted`/`Resolved` only — seat-overage grace/enforcement |
| **Entitlements Service** | `membership-entitlements-q` | `TenantMembershipsPurged` only (CD-25) — purge per-tenant snapshots on offboarding |
| **Usage & Metering** | `membership-usage-q` | `TenantMembershipsPurged` only (CD-25) — purge raw usage events on offboarding |
| **Delegation Service** | `delegation-cascade-q` | `MembershipRevoked`, `TenantMembershipsPurged` — async end of delegation rows on removal/offboarding |
| **Tender ACL Service** | (queue name owned by them) | `MembershipRevoked`, `TenantMembershipsPurged` — async cascade-delete of ACL overlays |
| **Group Mapping / JIT Config Service** | (queue name owned by them) | `TenantMembershipsPurged` — async wipe of group→dept/role mapping rows |
| **LLM Ingestion Service** (**proposed**) | (name TBD — OI-82) | `TenantMembershipsPurged` — purge ingestion corpus on tenant offboarding |
| **LLM Retrieval & Generation** (**proposed**) | `membership-rag-q` (CD-44) | `TenantMembershipsPurged` — purge artefacts, retrieval sets, reports on offboarding (OQ-29/OQ-75); `TenantStateChanged` — freeze/thaw collections on tenant status changes (OQ-71) |
| **Realm Provisioner** | `tenant-realm-q` | `TrialStarted` on `iam.tenant.events` — seeds RP's own trial-expiry sweep |
| **Notification** | `tenant-notification-q` | `TenantCreated`, `TrialStarted` on `iam.tenant.events` — welcome + trial-start emails |

### 4. Infrastructure dependencies

| System | Role |
|---|---|
| **PostgreSQL** (`org_membership` DB, shared RDS Multi-AZ) | Primary store — 10 tables, `FORCE ROW LEVEL SECURITY`, PgBouncer transaction pooling |
| **Valkey** (Redis-compatible) | Advisory cache (`om:memberships:*`, 300 s ± 30 s jitter) — a miss or outage falls through to Postgres, never a hard failure |
| **AWS SNS** | Two topics: `iam.membership.events` (12 event types via `RoutingPublisher`) and `iam.tenant.events` (2 event types) |
| **AWS SQS** | `tenant-orgm-q`, `billing-orgm-q`, and `catalog-orgm-q` (each with a `-dlq`, `maxReceiveCount=5`); `catalog-orgm-q` subscribes to Catalog's `iam-catalog-events` with filter `[DepartmentCatalogChanged, PlanCatalogChanged]` (SRE/platform Terraform) |
| **AWS Glue Schema Registry** | `iam-membership-events` and `iam-tenant-events` — version UUIDs resolved once at startup by definition (publish); consumers only strip the header, and validate against embedded consumed schemas |

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
| [`.claude/database-schema.md`](.claude/database-schema.md) | 10 tables, enums, RLS/tenant/seat/migration/trigger invariants |
| [`.claude/api-caching-events.md`](.claude/api-caching-events.md) | Full endpoint catalogue, cache TTLs, event catalogue, status-code table |
| [`.claude/request-flows.md`](.claude/request-flows.md) | Provisioning, delegate-impact resolution, invite→accept, concurrency, GDPR |
| [`.claude/operations.md`](.claude/operations.md) | Security, observability, configuration, CI/CD, dependency degradation matrix |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | Detailed architecture narrative with diagrams |
| [`docs/lld/iam-lld-org-membership-service.md`](docs/lld/iam-lld-org-membership-service.md) | Full LLD v2.38 — §16 open-question register, §17 error taxonomy, §19 migration strategy |
| [`docs/observability/`](docs/observability/README.md) | Observability standard implementation: identity, tiers, CI enforcement, SLOs; generated [metric inventory](docs/observability/metric-registry.md); [renamed metrics](docs/observability/migration.md); [alert runbooks](docs/observability/runbooks.md) |
| [`docs/runbook-schema-registry.md`](docs/runbook-schema-registry.md) | Schema-governance operator runbook |
| [`docs/runbook-outbox-dead-letter.md`](docs/runbook-outbox-dead-letter.md) | Inspecting and reprocessing `outbox_dead_letters` |
| [`docs/runbook-i8-latency.md`](docs/runbook-i8-latency.md) | I-8 hot-path latency runbook |
| [`docs/runbook-rls-cross-tenant.md`](docs/runbook-rls-cross-tenant.md) | RLS cross-tenant violation runbook |
| [`docs/architecture/`](docs/architecture/README.md) | Standalone Mermaid diagrams |
| [`test.md`](test.md) | Manual testing cookbook, incl. reading events from SQS |

---

## License / ownership

BCBP Solutions FZC LLC — internal platform service. Not for external distribution.
