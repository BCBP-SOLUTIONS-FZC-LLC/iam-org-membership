# Operations

## 10. Security

### 10.1 Tenant Isolation — Three Layers

- **Layer 1** — Keycloak realm boundary.
- **Layer 2** — PostgreSQL RLS with `app.tenant_id` GUC, `ENABLE` + `FORCE ROW LEVEL SECURITY`, `WITH CHECK`. Every policy — `tenants` included — routes both `USING` and `WITH CHECK` through `rls_check_tenant(tenant_id, 'table')` (`rls_check_tenant(id, 'tenants')` on the root table, single-row visibility), not a bare `current_setting(...)::uuid` comparison — see database-schema.md's Row-Level Security section for the full mechanism and why that distinction matters (write-side violations get sampled-logged too, via `rls_violation_log`/Layer 3 below).
- **Layer 3** — Audit-tagged cross-tenant detection via `rls_violation_log` + CloudWatch alarms.

### 10.2 Network Isolation

- `/api/v1/internal/*` — Kubernetes NetworkPolicy; only IAM-namespace service accounts reach it; public Envoy does not route.
- `/api/v1/operator/*` — Same network-layer isolation (§16 C1, AUTH-7): served **only** via operator ingress; NetworkPolicy blocks from tenant-facing network. Even a spoofed `x-tenant-roles: platform_operator` **cannot reach an operator route from public network**.

### 10.3 Input Validation

`slug` — DNS-label rules enforced by `provisioning_service.go`'s `isValidSlug` (a length check `3 ≤ len ≤ 63` combined with `slugRe = ^[a-z0-9]([a-z0-9-]{1,61}[a-z0-9])?$`: lowercase alphanumeric + hyphens, no leading/trailing hyphen), immutable after set (`trg_tenant_slug_immutable`). `default_locale` — a lightweight/structural BCP-47 check (`localeRe = ^[a-zA-Z]{1,8}(-[a-zA-Z0-9]{1,8})*$`), not full BCP-47 tag validation (a Phase 6 TODO per the code's own comment). `role_level`/`role_code` — ENUM. All UUIDs validated at handler layer (`uuid.Parse` on path/body params). **No `scope` field or `keycloak_group_name` length/regex validation exists in this repo any more** — both were leftovers from before ADR-0007/ADR-0008: `scope` belonged to the now-Delegation-Service-owned `delegation_scope` enum, and `keycloak_group_name` is received read-only from Group Mapping Service's JIT resolution response (`group_mapping_service.go` only string-compares it against the SAML assertion's own group names — no length/format check on this service's side, that validation is Group Mapping Service's concern now). `RegisterValidators()` (`middleware.go`) is a documented no-op stub ("Phase 0 leaves the set empty") — the validation above lives in service-layer functions, not Gin binding validators, despite the stub's comment listing "slug regex, keycloak group name, BCP-47 locale, mfa_freshness range" as its eventual scope.

### 10.4 Authorization Matrix

| Action | Required roles |
|---|---|
| Read tenant details | Any authenticated tenant member |
| Update tenant name/locale/local_accounts_enabled/mfa_freshness_seconds | `tenant_owner` |
| Invite user (P-6) / list-revoke pending invitations (P-30/P-31) | `tenant_admin`, `tenant_owner` |
| Remove (P-8), suspend/reactivate (P-7) | `tenant_admin`, `tenant_owner` |
| Grant/revoke tenant-level role (P-28) | `tenant_admin`, `tenant_owner`; last-owner protected (TM-8) |
| Assign user to dept (P-10/P-11) | `tenant_admin`, `tenant_owner` |
| Membership-existence check (I-15) | Internal service only (NetworkPolicy) — Tender ACL Service, Delegation Service |
| Provision tenant (I-1) | Internal service only (NetworkPolicy) |
| Activate/deactivate tenant dept (P-24/P-25) | `tenant_admin`, `tenant_owner` |
| Resolve blocked removal/demotion (P-26) | `tenant_admin`, `tenant_owner` (WFI-4) |
| View seat usage (P-27/I-11) | `tenant_admin`, `tenant_owner`; Billing internally |
| Change `licensed_seats` | Billing only via `TenantSeatsChanged` — no O&M endpoint (SEAT-4) |
| Set/clear `feature_flags` overrides (O-4) | `platform_operator` only (T-9/OP-6) |
| Reassign owner of ownerless tenant (O-7) | `platform_operator` only (T-13) |

Delegation create/cancel/reassign authorization (formerly "any tenant member" / `tenant_admin`+`tenant_owner`)
and tender-ACL grant authorization (formerly `tender_admin`+`tenant_admin`+`tenant_owner`) now belong to the
Delegation Service's and Tender-ACL Service's own LLDs — those routes are retired here (P-18/19/20/32/33,
P-21/22/23). System-department CRUD (O-1/O-2) and plan-catalog read/edit (O-5/O-6) authorization likewise
moved to the Catalog / Admin Config Service's own LLD (those operator routes are retired here too).

## 11. Observability

### 11.1 SLOs (SLO-1..3)

| Endpoint | p99 |
|---|---|
| I-8 (`GET /internal/users/:id/memberships`) cache hit | 15 ms |
| I-8 cache miss | 30 ms |
| `GET /tenants/:id/members` | 30 ms |
| `POST /tenants/:id/members` (P-6, incl. SEAT-1 `FOR UPDATE`) | 100 ms |
| `DELETE /tenants/:id/members/:user_id` (incl. `GetDelegateImpact`) | 200 ms |
| `POST .../removal-resolution` (reassign/cancel + re-validation, 2 round trips) | 350 ms |
| P-7 suspend (incl. advisory `GetDelegateImpact`, fail-open) | 150 ms |
| **Outbound event publish half** (outbox commit → SNS publish) — **O&M-owned** | 1 s |
| **Inbound lifecycle projection freshness** (producer publish → `tenants` reflects) | 30 s |

- **SLO-1** — Latency measured at API boundary, includes all synchronous work (cache/DB + any downstream call blocked on).
- **SLO-2** — Event end-to-end is joint budget with clear split. O&M owns publish half only; delivery+consume half owned by Workflow Service. O&M correctness never depends on 5 s — at-least-once outbox means slow consume delays timeliness, never loses events.
- **SLO-3** — Inbound projection freshness explicitly alerted. Measured via `iam_lifecycle_event_lag_seconds`. **Primary drift signal** because EVT-14 skips stale events **silently** — lag alert is the drift signal, not DLQ.

### 11.2 Prometheus Metrics

Registered once in `metrics.Register(environment)` (`internal/adapter/outbound/metrics/business.go`) onto gincommon's registerer (same registry as HTTP metrics), per the **IAM Platform Observability Standard**'s three-tier hierarchy. `domain`/`service`/`environment` are injected centrally via `platformLabels`/`serviceLabels` (ConstLabels) — never left to an individual `Inc()`/`Observe()` call site (rule 8):

- **Tier 1 — `platform_*`** (required labels: `domain`, `service`, `environment`): a concept common across domains (IAM, Workflow, Billing, Tender Management, ...) with identical semantics. `domain="iam"` is a fixed const label on every Tier-1 collector this service registers.
- **Tier 2 — `iam_*`** (required labels: `service`, `environment`): a concept shared across multiple IAM-domain services but not meaningful outside IAM. No `domain` label — the namespace already encodes it.
- **Tier 3 — `iam_org_membership_*`**: behavior unique to this one service. No other IAM service has an equivalent concept, so the service identity is baked into the name.

Cardinality-bounded: no `tenant_id`/`user_id`/`email` label anywhere (§16 A48) — every label is a small, fixed enum.

**Tier 1 — `platform_*`**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `platform_messages_received_total` | Counter | `queue` | Inbound SQS message dequeued, before processing — wired in `cmd/server/main.go`'s `instrumentedHandler`, around both `tenant-orgm-q` and `billing-orgm-q` |
| `platform_messages_processed_total` | Counter | `queue` | Inbound SQS message whose `Handle` returned nil |
| `platform_messages_failed_total` | Counter | `queue` | Inbound SQS message whose `Handle` returned an error (includes DLQ rejections) |
| `platform_dlq_messages_total` | Counter | `event_type`, `reason` | Events actively rejected to DLQ without recording `processed_events`. Currently `reason="future_time_clamp"` only (EVT-15) — **any nonzero rate pages** (producer clock skew). Canonical per the standard's own plain Tier-1 Examples list — no ratification needed |
| `platform_duplicate_messages_total` | Counter | `consumer` | SQS redelivery filtered by `processed_events` PK (IDEMP-4). **Status: Proposed** (`registry.go` `PlatformRegistry`) — named in the standard's own Registry-Proposed Examples but not yet ratified. Shadow-emitted only; `iam_org_membership_processed_events_duplicates_total` below is the authoritative alerting/SLO source until ratified |
| `platform_dependency_request_seconds` | Histogram | `target_service` (`catalog`\|`group_mapping`\|`delegation`), `endpoint` | Latency of the three ADR-0007/ADR-0008 synchronous cross-service calls (buckets 5ms–3s). `target_service` is the downstream peer, distinct from the `service` const label (this service's own identity). **Status: Proposed** — shadow-emitted only; `iam_org_membership_dependency_call_duration_seconds` below is authoritative until ratified |
| `platform_dependency_errors_total` | Counter | `target_service`, `endpoint`, `outcome` (`5xx`\|`timeout`\|`fallback_served`) | Cross-service call failure; `fallback_served` is recorded by the calling `CatalogService`/`GroupMappingService`, not the client. **Status: Proposed** — and unlike the two metrics above, not even one of the standard's own named examples (added by symmetry, see `registry.go`). Shadow-emitted only; `iam_org_membership_dependency_call_failures_total` below is authoritative until ratified |

`platform_queue_depth`/`platform_dlq_depth` (SQS `ApproximateNumberOfMessages*`) and `platform_retry_total` (the outbox publisher's own retry-on-publish-failure loop, which lives inside the vendored `platform-events` library, not this repo) are **not implemented** — the former needs a new SQS `GetQueueAttributes` polling goroutine (new AWS call + IAM permission, not added without an explicit decision) and the latter isn't ours to instrument from here.

**Tier 1 candidates, shadow-emitted (Status: Proposed, `registry.go`)** — dual-emitted from the same call site as their Tier-1 counterpart above, but these Tier-3 names are what this repo's alerts/recording-rules/SLOs actually query until ratification flips a `PlatformRegistry` entry's `Status` to `StatusCanonical` (mirrors `iam-realm-provisioner`'s pattern):

| Metric | Type | Labels | Supersedes |
|---|---|---|---|
| `iam_org_membership_dependency_call_duration_seconds` | Histogram | `target_service`, `endpoint` | `platform_dependency_request_seconds` |
| `iam_org_membership_dependency_call_failures_total` | Counter | `target_service`, `endpoint`, `outcome` | `platform_dependency_errors_total` |
| `iam_org_membership_processed_events_duplicates_total` | Counter | `consumer` | `platform_duplicate_messages_total` |

**Tier 2 — `iam_*`**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `iam_rls_violations_total` | Counter | `violation_type` | **Still registered but never incremented** (`cmd/server/exporters.go`'s 4 goroutines cover only the Tier-3 gauges below, none scrape `rls_violation_log`). The query side of the fix landed 2026-09-20 — `pgadapter.GaugeRepository.RLSViolationCounts(ctx, window)` (BYPASSRLS `sysPool`, `GROUP BY violation_type` over a trailing window), verified against real Postgres (`test/postgres/gauge_repository_test.go`'s `TestGaugeRepo_RLSViolationCounts_GroupsByTypeWithinWindow`) and intended to back a 5th exporter goroutine mirroring `iam-user-profile`'s `runRLSViolationExporter` — but wiring it into `cmd/server/exporters.go`'s ticker loop is deliberately not yet done: that file's exporter-construction signature is also being rewritten by an in-flight, uncommitted `platform-events`/`eventcfg` migration, and wiring this in ahead of that would need reverting once it lands. Treat the gauge as real/tested, the counter as still not live. |
| `iam_auth_session_revoke_failed_total` | Counter | `reason` | RP `RevokeUserSessions` non-2xx/error (AUTH-8 fail-open). Grouped under `iam_auth_` rather than `iam_org_membership_` since any IAM service reducing a user's privilege via RP session-revoke can emit this identically |
| `iam_lifecycle_event_skipped_total` | Counter | `event_type` | EVT-14 recency-guard skip (post-DLQ-redrive spikes are expected). Domain-shared: any IAM service consuming RP-produced tenant-lifecycle events with the same last-writer-wins guard (Delegation, Tender-ACL, Group-Mapping) can emit this identically |
| `iam_lifecycle_event_lag_seconds` | Histogram | `event_type` | Seconds between `event.time` and consumer apply — **SLO-3 primary drift signal** (buckets 0.05s–1800s) |

**Tier 3 — `iam_org_membership_*`**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `iam_org_membership_unknown_event_acknowledged_total` | Counter | `topic`, `event_type` | Consumed event of a type this service doesn't handle — silently ack'd; sustained nonzero means a producer added a new type |
| `iam_org_membership_delegate_suspend_impact_total` | Counter | `checked` | P-7 suspend WFI-13 advisory fired (§8.8.5, never a block) |
| `iam_org_membership_tenant_ownerless_escalated_total` | Counter | `reason` | Event-time signal: a removal just dropped the last active `tenant_owner` (TM-12) — every increment should page |
| `iam_org_membership_seat_overage_started_total` | Counter | `cause` | Under-cap → over-cap transition (SEAT-5) |
| `iam_org_membership_seat_limit_reached_total` | Counter | `plan` | P-6 invite blocked by SEAT-1 cap |
| `iam_org_membership_invite_throttled_total` | Counter | `reason` (`cooldown`\|`rate_limit`) | Pre-RP-call throttle (PI-11/PI-12) |
| `iam_org_membership_realm_sync_failed_total` | Counter | `stage` | `realm-config-sync` reconciler failure (T-15, security-relevant on disable) |
| `iam_org_membership_delegate_removal_blocked_total` | Counter | `scope` | `409 workflow_resolution_required` (WFI-3) |
| `iam_org_membership_delegate_reassignment_total` | Counter | `action` (`replace_delegate`\|`stop_workflows`) | P-26 removal-resolution completion |
| `iam_org_membership_membership_exists_check_total` | Counter | `caller`, `result` | I-15 grant-time existence checks served (Tender ACL Service, Delegation Service) — only this service owns membership data, so this can't be domain-shared |
| `iam_org_membership_tenant_ownerless` | Gauge | — | `count(*) WHERE ownerless_since IS NOT NULL` (T-13) — 5-min exporter; sustained >0 pages `platform_operator` |
| `iam_org_membership_realm_sync_pending` | Gauge | — | `count(*) WHERE realm_sync_pending` (T-15) — 5-min exporter |
| `iam_org_membership_seat_overage_active` | Gauge | — | `count(*) WHERE overage_since IS NOT NULL` (SEAT-5) — 5-min exporter |
| `iam_org_membership_pending_invitations_stale` | Gauge | — | Pending invitations past `expires_at` that `invitation-expiry` hasn't flipped yet — 5-min exporter |

**Not `iam_`/`platform_`-namespaced (library-owned, out of this standard's scope):** `outbox_dead_letters_total` (`platform-events`), `http_request_duration_seconds`/`http_requests_total` (`platform-gincommon`), `pgcommon_*` (`platform-pgcommon`).

**CI enforcement:** `.github/scripts/check-metric-naming.sh` (wired into `Validate / Quality`) statically greps `internal/adapter/outbound/metrics/business.go` for every `Name: "..."` string and asserts: the name matches `^(platform|iam)_[a-z0-9_]+$`; every `CounterVec`/`Counter` name ends `_total`; every `HistogramVec` name ends `_seconds`; a `platform_*` name's registration block sets both `"domain"` and `"environment"` keys in its ConstLabels literal; an `iam_*` (non-`iam_org_membership_`) name's block sets `"environment"` but not `"domain"`; an `iam_org_membership_*` name has no additional label requirement. It does not (and cannot, staically) verify the *classification judgment* (shared vs. service-specific) — that's a review-time call per the decision tree, not a lint rule.

**Registry ratification enforcement (rules 11/12):** `.github/scripts/metrics-registry-lint.sh` (also wired into `Validate / Quality`) reads `PlatformRegistry` (`internal/adapter/outbound/metrics/registry.go`) and fails if any entry with `Status: StatusProposed` appears as a live query target (inside an `expr:`) in `deploy/monitoring/app-alerts.yml`, `deploy/monitoring/slo-rules.yml`, or `deploy/helm/templates/prometheusrule.yaml` — a comment mentioning the name is fine, a PromQL selector/function using it is not. Mirrors `iam-realm-provisioner`'s own `registry.go`/script pair.

Not implemented (do not treat as current): `iam_membership_joins_total`/`_leaves_total`, `iam_memberships_cache_hit_ratio`, `iam_membership_lookup_latency_seconds`, `iam_invitations_created_total`/`_accepted_total`/`_expired_total`/`_revoked_total`, `iam_invite_kc_cleanup_pending`/`_failed_total`, `iam_group_mapping_resolution_errors_total`, `iam_delegate_workflow_cancel_total` (separate from `iam_org_membership_delegate_reassignment_total{action=stop_workflows}`, which already covers that case) — these were on an earlier planned metric surface that was never built; this whole line is a Phase-6 note, not a current gap list.

**Alerts:**
- `outbox_dead_letters_total rate > 0` → page
- `iam_org_membership_tenant_ownerless > 0` → page `platform_operator` (O-7 required)
- `platform_dlq_messages_total{reason="future_time_clamp"} rate > 0` → page (producer clock skew / bad replay)
- Sustained `iam_auth_session_revoke_failed_total` → page (AUTH-8 fast-kill degraded, only TTL-bounded)
- `iam_lifecycle_event_lag_seconds > 30` for ~2 min → page (SLO-3 breach, primary drift signal)
- `iam_org_membership_realm_sync_pending > 0` sustained beyond ~10 min → page (T-15)
- Sustained `iam_org_membership_realm_sync_failed_total` → page
- Sustained `iam_org_membership_invite_throttled_total` for one tenant → warn (email abuse / bad client)
- Sustained `iam_org_membership_delegate_removal_blocked_total` without matching `iam_org_membership_delegate_reassignment_total` → warn (admins hitting block, not completing resolution)
- Spike in `iam_org_membership_seat_limit_reached_total` for a tenant → **informational Slack to CSM/Billing** (not on-call — genuine "buy more seats" signal)
- `iam_org_membership_pending_invitations_stale > 0` sustained → warn (`invitation-expiry` cron not keeping up)
- Sustained `iam_org_membership_dependency_call_failures_total{target_service=catalog}` → warn (the one ADR-0007/ADR-0008 dependency that is NOT fail-open — see §20.7). Query the Tier-3 predecessor, not `platform_dependency_errors_total` (Status: Proposed, shadow-emitted only — see the registry ratification note above)

**Metric naming (§16 A50/J4):** IAM Platform Observability Standard three-tier hierarchy — see §11.2 above. Shared (`platform_*`/`iam_*`) names are disambiguated by the `service`/`domain` const labels, never by encoding the service into the name; dashboards/alerts on shared names aggregate `by (service)` (or `by (domain, service)` for Tier 1), not by the Prometheus `job` label, which HTTP-only metrics still use.

**Cardinality guardrails (§16 A48):** no unbounded / user-supplied label (`user_id`, `email` etc.) may be added. If tenant count grows past ~10k, highest-churn counters drop `tenant_id` in favor of structured logs / OTel exemplars; low-cardinality gauges keep it.

### 11.3 OTel Tracing

`platform-gincommon.InitTracingFromEnv()` for the process-global `TracerProvider`. **There is no exported `platform-pgcommon.NewOTelQueryTracer`** — `pgcommon.Config.Tracer` (type `port.Tracer`, a one-method `StartSpan(ctx, name) (context.Context, func())` interface) is the extension point, and pgcommon wraps whatever's set there internally as its own unexported `otelQueryTracer`/`multiTracer` `pgx.QueryTracer`. This service supplies that interface itself: `internal/adapter/outbound/postgres/otel_tracer.go`'s `NewOTelTracer(serviceName)` returns an `otelTracer` backed by `otel.Tracer(serviceName)`, wired onto `Config.Tracer` for **both** the app pool and the BYPASSRLS `sysPool` in `cmd/server/main.go` and `cmd/reconciler/main.go` — so every `db.query` span exports through the same OTLP pipeline as HTTP spans on either binary. Cross-service client spans (`catalogadmin`/`groupmappingclient`/`delegationcheck`/`workflow`/`realmprovisioner`/`tokenservice`) carry child spans for the outbound HTTP call + DB write where applicable. W3C `traceparent` propagation is **not** `gincommon.PropagateHeaders` (that's the inbound-focused helper) and, as of this pass, **not** a hand-rolled per-package `propagateTraceparent` helper either — `internal/adapter/outbound/httpx/transport.go`'s `httpx.NewClient(timeout)` (an `otelhttp`-wrapped shared `http.Client` factory) is now built by all six outbound clients, verified by direct grep with zero leftover manual traceparent code anywhere in that package set; `otelhttp.NewTransport` handles injection + client-span emission automatically. (A prior version of this note described `httpx` as unadopted scaffolding — that was already stale before this pass; the migration predates it and is fully committed.)

### 11.4 Structured Logs (Zap)

Slow queries > 200 ms at WARN (`tenant_id` redacted). RLS violations at ERROR (1% sampled). Delegate-impact events at INFO. `tenant_ownerless_escalation` at ERROR (durable, page-worthy record from I-5 cascade); `tenant_owner_reassigned` at INFO (O-7 clear side).

## 12. Configuration

Table below is verified directly against the current `.env-example` (not just prose) — variable names, not just values, were wrong in a prior version of this table (`SNS_TOPIC_ARN_MEMBERSHIP`/`SNS_TOPIC_ARN_TENANT`/`SQS_QUEUE_URL_TENANT_EVENTS`/`SQS_QUEUE_URL_BILLING_EVENTS`/`OUTBOX_POLL_INTERVAL_MS`/`OUTBOX_DRAIN_TIMEOUT_S`/`OUTBOX_STARTUP_JITTER_S` do not exist; `VALKEY_TIMEOUT_MS` does not exist — the Valkey client hardcodes 50 ms read/write / 100 ms dial with no env override).

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` or `PG_HOST`/`PG_PORT`/`PG_USER`/`PG_PASSWORD`/`PG_DBNAME`/`PG_SSLMODE` | — | Postgres DSN for `org_membership` (one or the other) |
| `PG_MAX_CONNS` / `PG_MIN_CONNS` | `20` / `0` | Pool sizing per pod |
| `PG_SLOW_QUERY_THRESHOLD` | `200ms` | WARN log threshold (`tenant_id` redacted) |
| `PG_BOUNCER_MODE` | `true` (dev) | `true` in prod/staging |
| `MIGRATION_DATABASE_URL` | (required) | Direct Postgres DSN for migrations (bypasses PgBouncer, CONFIG-2) |
| `SYSTEM_DATABASE_URL` | — | BYPASSRLS `sysPool` DSN (`org_membership_migrator`) — reconciler jobs, business-metric exporters, I-16. Falls back to the app DSN in dev with a startup warning (cross-tenant queries then RLS-filter to 0 rows) |
| `VALKEY_URL` | `localhost:6379` | Cache endpoint — no separate timeout env var (see above) |
| `SNS_TOPIC_MEMBERSHIP_ARN` | (required outside dev) | `iam.membership.events` |
| `SNS_TOPIC_TENANT_ARN` | (required outside dev) | `iam.tenant.events` |
| `SQS_TENANT_ORGM_QUEUE_URL` | (required outside dev) | `tenant-orgm-q` |
| `SQS_BILLING_ORGM_QUEUE_URL` | (required outside dev) | `billing-orgm-q` |
| `SQS_CATALOG_ORGM_QUEUE_URL` | — (unset everywhere today) | `catalog-orgm-q` ← `DepartmentCatalogChanged` (Gap 12) — consumer done/tested, queue inert until this is set **and** `iam-catalog-admin` ships a publisher |
| `SQS_TENANT_ORGM_CONCURRENCY` / `SQS_BILLING_ORGM_CONCURRENCY` / `SQS_CATALOG_ORGM_CONCURRENCY` | `4` / `2` / `2` | Per-queue consumer concurrency, overlaid onto `platform-events`' shared `eventcfg.LoadSQS()` env contract (`cmd/server/wiring.go`'s `sqsEnvForQueue`) |
| `OUTBOX_RETENTION_DAYS` | `8` | `outbox-prune` CronJob retention, consumed by `outbox.Runner.PrunePublished` |
| `GLUE_REGISTRY_MEMBERSHIP_NAME` / `_ARN` | — | `iam-membership-events` registry — unset ⇒ `NoopCodec` (plain JSON) on that topic |
| `GLUE_REGISTRY_TENANT_NAME` / `_ARN` | — | `iam-tenant-events` registry (**shared with Realm Provisioner** — disjoint schema names) — unset ⇒ `NoopCodec` on that topic |
| `WORKFLOW_SERVICE_BASE_URL` / `WORKFLOW_TIMEOUT_MS` | — / `3000` | §8.8 delegate-impact/reassign/cancel — fail-closed |
| `REALM_PROVISIONER_BASE_URL` / `REALM_PROVISIONER_TIMEOUT_MS` | — / `3000` | Invited-user create/delete, realm-config patch, session revoke, MFA reset |
| `CATALOG_ADMIN_BASE_URL` / `CATALOG_ADMIN_TIMEOUT_MS` | — / `3000` | Catalog / Admin Config Service (ADR-0007 Wave 1) — `om:plans`/`om:departments` source. **Not fail-open**: cold-cache-plus-failure surfaces `catalog_unavailable` (503) |
| `GROUP_MAPPING_BASE_URL` / `GROUP_MAPPING_TIMEOUT_MS` | — / `300` | Group Mapping / JIT Config Service (ADR-0007 Wave 2) — I-10 SAML group→dept/role resolution. Fails **open**: cold-cache-plus-failure serves an empty resolution rather than blocking login |
| `DELEGATION_BASE_URL` / `DELEGATION_TIMEOUT_MS` | — / `1000` | Delegation Service (ADR-0008) — §8.8.4 dept-scope delegate pre-filter. Fails **open**: degrades to tenant-wide delegate-impact scoping. Raised from 300ms (Gap-8 fix: too tight under load/cold-start, caused frequent fallback) |
| `TOKEN_SERVICE_BASE_URL` / `TOKEN_SERVICE_TIMEOUT_MS` | — / `1000` | Token Service (AUTH-9/IB-3) — `GET /service-accounts?principal_sub=` (TS-5) service-account-not-grantable check on membership-create (P-6/I-3) and role-grant (P-10/P-28). Fails **open**: an empty/unreachable value degrades to allowing the operation, since the primary guarantee is structural (composite FK bar, TR-8/DM-4) |
| `INVITATION_EXPIRY_DAYS` | `7` | Pending invitation window; **must equal Keycloak invite action-token lifespan** |
| `INVITE_REINVITE_COOLDOWN_MINUTES` | `15` | Per-email cooldown (PI-11); `0` disables |
| `INVITE_MAX_PER_TENANT_PER_HOUR` | `60` | Per-tenant hourly ceiling (PI-12); `0` disables |
| `SEAT_OVERAGE_GRACE_DAYS` | `30` | Drives `grace_ends_at` + past-grace alert. **Not** an auto-action trigger (SEAT-3/SEAT-4) — Billing's enforcement decision |
| `SUBSCRIPTION_GRACE_DAYS` | `30` | I-16's (§16 OQ-9/RP-C3) cancellation-to-suspension threshold — `ListSubscriptionLapses` filters `cancelled_at` against this. O&M owns the value so RP's sweep isn't a second, driftable copy of the same rule |
| `MAX_LIFECYCLE_EVENT_SKEW_SECONDS` | `300` | EVT-15 future-time clamp threshold |
| `OUTBOX_POLL_INTERVAL` | `500ms` | Go duration string, not a bare ms integer |
| `OUTBOX_BATCH_SIZE` | `50` | Publish batch size |
| `OUTBOX_MAX_ATTEMPTS` | `5` | DLQ threshold (EVT-5) |
| `OUTBOX_DRAIN_TIMEOUT` | `30s` | Shutdown drain |
| `OUTBOX_PUBLISH_CONCURRENCY` | `4` | Concurrent publish workers |
| `OUTBOX_PUBLISH_TIMEOUT` | `10s` | Per-publish timeout |
| `OUTBOX_STARTUP_JITTER` | `2s` | Avoids every replica's runner waking in lockstep |
| `OUTBOX_CLAIM_LEASE_DURATION` | `10m` | Claimed-row lease before another replica can retry it |
| `PROCESSED_EVENTS_TTL_DAYS` | `8` | Consumer dedup retention (PE-1, > 7-day SQS max lifetime) |
| `CACHE_TTL_SECONDS` | `300` | Base TTL for `om:*` keys |
| `DOCS_ENABLED` / `DOCS_AUTH_TOKEN` | `false` / — | Swagger UI + AsyncAPI viewer gating in production |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — (opt-in) | OTLP collector — unset means spans stay in-process only |
| `BUILD_VERSION` | (CI-injected) | `-ldflags` at build time |

**CONFIG-1..5:** All infra endpoints env-supplied (no compiled config, only `BUILD_VERSION`). Same image runs everywhere.

## 13. Deployment & Scaling

Helm chart mirrors `iam-user-profile`. `terminationGracePeriodSeconds = 75`. HPA (`deploy/helm/templates/hpa.yaml`, `autoscaling.*` in `values.yaml`): 2–8 replicas, scaling on CPU (`targetCPUUtilizationPercentage: 70`) + optional memory (`targetMemoryUtilizationPercentage: 75`) + an optional custom-metric RPS-per-replica target (`targetRPSPerReplica: 500` against `http_requests_per_second`, silently absent from the HPA — falling back to CPU/memory only — unless `prometheus-adapter` is installed with `deploy/monitoring/prometheus-adapter-rule.yaml`). **Not** `iam_memberships_cache_hit_ratio` — that metric isn't implemented (§11.2) and was never wired into the HPA. PDB `minAvailable: 1`. Resources (`values.yaml`): requests CPU 100m / Memory 256Mi, limits CPU 500m / Memory 512Mi.

**Pod spec additions** (`deploy/helm/templates/deployment.yaml`/`cronjobs.yaml`, purely additive — `helm lint` passes clean, no change to the security posture below): `imagePullSecrets` (private registry pulls), `nodeSelector`/`tolerations` (node placement), `podLabels` (extra labels merged onto pod metadata), and a `tmp` `emptyDir` volume mounted at `/tmp` — required because `securityContext.readOnlyRootFilesystem: true` otherwise leaves no writable `/tmp` for the process. All default to empty/off in `values.yaml` and only take effect when a deploying environment sets them.

### 13.1 CronJobs

Schedules verified directly against `deploy/helm/values.yaml`'s `cronjobs:` map (a prior version of this table had every schedule wrong except `trial-cleanup`):

| CronJob | Schedule | Purpose |
|---|---|---|
| `invitation-expiry` | `*/5 * * * *` | Past-`expires_at` pending → `expired` + `kc_cleanup_pending=true` (PI-5/PI-9) |
| `invitation-kc-cleanup` | `*/10 * * * *` | Saga-compensation reconciler (PI-9): sweep `kc_cleanup_pending`, call `RealmProvisioner.DeleteUser`, clear flag |
| `realm-config-sync` | `*/10 * * * *` | Realm-config reconciler (T-15): sweep `realm_sync_pending`, call idempotent `PatchRealmConfig`; **prioritises disables** (security-tightening) |
| `seat-overage-reconcile` | `0 */6 * * *` | Seat-overage marker backstop (SEAT-5): recompute `overage_since`; also drives past-grace alert |
| `trial-cleanup` | `0 2 * * *` | Phase-2 DB executor: soft-delete + PII-scrub for `trial_expired` past 15-d grace (§15.3) |
| `outbox-prune` | `0 3 * * *` | Batched delete at `OUTBOX_RETENTION_DAYS` (default 8) retention. **Migrated (uncommitted, in progress as of 2026-09-20) from the local `port.ReconcilerStore.PruneOutbox` hand-rolled batched-SQL delete to `platform-events`' own `outbox.Runner.PrunePublished(ctx, retention, batchLimit)`** — `cmd/reconciler`'s job now builds a real (never-`Start()`ed, publisher-less) `*outbox.Runner` via the same `eventcfg.LoadOutbox()`/`RunnerConfigFromEnv()` contract `cmd/server` uses, purely to call this one method. **Regression found and fixed 2026-09-20**: the migrated `OutboxPrune` job body correctly no-ops (Succeeded=0, no error) when `jctx.OutboxRunner` is nil — but 3 `test/postgres` integration tests (`TestReconciler_OutboxPrune_BatchLimitCapsOneTick`, `TestReconciler_OutboxPrune_DeletesPublishedPastRetention`, `TestREC_OUTBOX_001_PruneDropsOnlyRowsPastRetention`) were never updated for the new dependency and constructed `jobs.Context` without setting `OutboxRunner` at all, so they were silently exercising the no-op skip path, not the real prune logic — not a bug in `cmd/reconciler/main.go`'s actual production wiring (which already built and wired `OutboxRunner` correctly). Fixed by wiring a real `*outbox.Runner` (`Pool: sysPool`, `Publisher: eventbusadapter.NoopPublisher{}`) into the shared `newJobContext` test helper and both standalone `TestREC_OUTBOX_00*` tests; all 4 outbox-prune tests pass against a real Postgres container, full `test/postgres` suite re-verified clean. `port.ReconcilerStore.PruneOutbox` itself — confirmed dead (zero production callers) once the `PrunePublished` migration lands — has its removal (interface method, implementation, dedicated tests, `fakeReconcilerStore` fields) prepared and verified in the working tree, but **not yet committed**: it's entangled with the still-uncommitted `platform-events`/`eventcfg` migration above and would break the build if committed alone. See the commit-sequencing note in the LLD's rev 2.6 changelog entry and the "Events / Outbox / Dedup Rule" notes in `.claude/architecture.md` |
| `processed-events-prune` | `0 4 * * *` | Batched delete via `port.ReconcilerStore.PruneProcessedEvents` at `PROCESSED_EVENTS_TTL_DAYS` (8) |

Exactly **7** CronJobs, dispatched via `cmd/reconciler/main.go --job=<name>` (verified against `cmd/reconciler/jobs/` and `main.go`'s `registry` map). All cross-tenant reconciler queries now go through `port.ReconcilerStore` (`internal/adapter/outbound/postgres/reconciler_store.go`, `sysPool`-bound) rather than the jobs issuing raw SQL directly. `quota-reset` and `quota-utilization-metrics` **removed** (§16 A26 — moved to Usage & Metering). `delegation-expiry`, `delegation-review`, `delegation-cleanup` (→ Delegation Service) and `acl-cleanup` (→ Tender ACL Service) **removed** by the ADR-0007/ADR-0008 decomposition — those tables and their lifecycle no longer live in this database.

### 13.3 Migration Safety

Rolling deploy, 3 replicas. `migrate.Runner` `lock_timeout=30s`. Additive changes zero-downtime. `UNIQUE` via `CREATE UNIQUE INDEX CONCURRENTLY` + `ADD CONSTRAINT ... USING INDEX` in separate releases (MIG-8).

## 14. Testing Strategy

**Coverage:** merged statement coverage (`make test-ci` → `coverage.out`, `make cover-func` for a per-function breakdown) currently sits at **99.1%** (verified via a fresh `make cover-func` run, 2026-09-20 — up from a previously-documented 98.4%, after the AUTH-9/IB-3 test suite and the outbox-prune test fix both landed), comfortably above the CI gate's 95% floor (`.github/scripts/coverage-gate.sh`, `COVERAGE_THRESHOLD` env var, default 95, not overridden in `.github/workflows/validate-test.yml`). The Makefile's `TEST_INTERNAL_PKGS` (white-box tests folded into `test-unit`/`_test-unit`/`race`) must explicitly list every package under `internal/`/`pkg/` that carries its own `_test.go` files — `-coverpkg=$(COVER_PKG_LIST)` instruments a package for coverage *accounting* but does not make `go test` actually *run* a package's tests unless that package also appears in the invoked package list. `./internal/core/port/...` and `./internal/adapter/outbound/httpx/...` were both missing from `TEST_INTERNAL_PKGS` until a recent fix, despite each having its own test file — their tests silently never ran under `make test-ci`/`make test`/CI even though the coverage number looked complete. Both are now included; treat any future new package under `internal/`/`pkg/` with its own `_test.go` as needing the same check.

- **§14.1 Unit** (`testify/mock` for `port.WorkflowClient`/`RealmProvisionerClient`/`CatalogAdminClient`/`GroupMappingClient`/`DelegationCheckClient`): group-mapping resolution (incl. fail-open empty-resolution path), catalog read-through + stale-if-error fallback (incl. fail-closed `catalog_unavailable`), seat-cap arithmetic (SEAT-1), idempotency keys, §8.8 delegate-impact resolution (blocked/proceed/replacement-validate/re-check), §8.8.5 fail-open advisory, §8.8.4 department extension (level increase never calls Workflow — `AssertNotCalled`, `scope='all'` specifically excluded WFI-10; dept-scope delegate pre-filter fail-open to tenant-wide scoping on `DelegationCheckClient` failure), §16 A11 invitation flow (stages-not-adds, lost-race compensation, duplicate detection, acceptance materialization, revoke frees + reconciles, throttling before RP call PI-11/PI-12). No `port.UserProfileClient` — that adapter was deleted as dead code once delegation's OOO coordination moved to the standalone Delegation Service.

- **§14.2 Integration (testcontainers-go, real PG + Valkey, full migration suite):**
  - RLS fail-closed (missing GUC / cross-tenant write / malformed GUC — all 0 rows or policy violation).
  - **Operator-route defense-in-depth (AUTH-7):** client-supplied `x-tenant-roles` header with `platform_operator` value → `403 insufficient_role` before any DB access.
  - `touch_row` fires only on real changes.
  - Composite membership FKs (§16 A15/A28/A31/A16 DEL-9/TAE-8) reject anchor mismatches.
  - EVT-14 recency + EVT-15 future-time clamp + EVT-16 relay (state-change emits, no-op/stale doesn't).
  - I-13 ineligible assignee is `422 assignee_ineligible` (not 409, §16 A62).
  - Last-owner deletion at identity layer escalates (TM-12/T-13, `ownerless_since` set, `iam_tenant_ownerless_total++`, ERROR log); actor path P-8 refuses `422 last_owner_removal`.
  - **§16 A46/AUTH-8** — session revocation on suspend/removal/deprivilege; fail-open on RP 5xx; I-5 hard-delete never calls (KC deletion kills sessions).
  - **§16 A58/T-15** — Realm-config propagation + durable reconcile (200 on inline OK, 202 on inline fail, reconciler converges, disables prioritized, idempotent PATCH).
  - **§16 A45/TR-9** — Removal soft-deletes ALL tenant_roles rows; one `TenantRoleRevoked` per grant; suspend leaves `tenant_roles` frozen.
  - **§16 A44/TM-13** — Concurrent owner-drops serialize (exactly one succeeds, other `422`).
  - **O-7** — Recovers ownerless; validates active-member candidate; 409 on offboarded tenant; 403 on non-operator caller.
  - **§16 A59 seat-overage** — Downgrade into overage stamps marker + emits; new invites blocked; existing keep access; resolution clears + emits `Resolved`; idempotent no-double-emit; backstop reconcile; O&M never enforces past grace.
  - **§16 A10 seat-cap** — 3rd invite at `licensed_seats=2` → `409 seat_limit_reached` with body fields matching P-27/I-11 response shape; concurrent P-6 with 1 remaining seat → exactly one succeeds (row-lock serializes); P-27 and I-11 return identical bodies.
  - **§16 A11 invitation flow** — pending counts toward cap; full invite→accept round-trip (`member` never stored, TR-7); revoke frees + `kc_cleanup_pending` reconciler; expiry frees seat before sweep; re-invite after terminal (PI-1 `uq_pi_pending` allows); duplicate live pending → `409 invitation_already_exists`.

- **§14.3 Contract tests** — Verify `port.WorkflowClient`/`RealmProvisionerClient`/`CatalogAdminClient`/`GroupMappingClient`/`DelegationCheckClient` HTTP shapes exactly; cover 5xx/timeout → `*_unavailable` mapping (Catalog fail-closed) and fail-open degradation (Group Mapping, Delegation-check).

- **§14.4 E2E / smoke (staging):** Full-stack tenant provisioning → member add → dept assign. Events on `iam.membership.events` within 5 s p99. Extended path covers §8.8/§8.8.4/§16 A10 end-to-end.

- **§14.5 RLS Case 5 (canonical, critical):** No cross-tenant GUC leak across a pooled PgBouncer backend. Pool pinned to single backend (`MaxConns=1`), tenant A tx → return connection → tenant B tx on same backend → assert B sees 0 of A's rows. Step 3 is decisive: even a mis-written non-transactional read must fail closed (0 rows), never inherit A's stale session GUC. CI additionally greps for non-`LOCAL` `SET app.tenant_id` as forbidden pattern.

## 18. Integration Points

There is no `port.UserProfileClient`/`adapter/outbound/userprofile` integration in this repo any more. The old `SetAvailability` OOO-coordination flow was dead code once delegation's OOO coordination moved to the standalone Delegation Service (zero remaining call sites), and the adapter plus its `USER_PROFILE_SERVICE_BASE_URL`/`USER_PROFILE_TIMEOUT_MS` env vars have been deleted (LLD §16 OQ-5).

### 18.2 `authz-enrichment`

- AuthZ → O&M: `GET /internal/users/:id/memberships` (I-8 hot path).
- O&M → events → AuthZ: `iam.membership.events` for cache invalidation.
- Billing/RP → events → AuthZ (direct): `TenantPlanChanged`, `TenantConverted` — AuthZ consumes these directly; O&M is NOT the producer of plan-change events.

### 18.3 `realm-provisioner`

- RP → O&M: `POST /internal/tenants` (I-1, actually called by the Signup BFF, not RP directly — confirmed by a full-repo grep of `iam-realm-provisioner` finding zero callers of either I-1 or I-2 from RP's own codebase). `PATCH /internal/tenants/:id` (I-2) similarly has **no real caller today** — the actual realm-identity propagation path is the `TenantRealmReady` **event** below, not a synchronous I-2 call. I-2 is registered and documented but appears to be dead/aspirational; worth either wiring a real caller or retiring it.
- O&M → RP: `POST /internal/tenants/:id/users` (invited-user create, §16 A11 — now sends an `Idempotency-Key` header, a bug fix: RP's route hard-requires one and this client never sent it before, so every real invite failed once `REALM_PROVISIONER_BASE_URL` was actually set); `DELETE /internal/tenants/:id/users/:keycloak_user_id` (compensating delete, PI-9 durable via `kc_cleanup_pending`); `PATCH /internal/tenants/:id/realm-config` (T-15 realm-config propagation, Option A local-first commit-then-call, durable via `realm_sync_pending`); `POST /internal/tenants/:id/users/:keycloak_user_id/revoke-sessions` (AUTH-8 session revocation, best-effort/fail-open — the actual path is `/revoke-sessions`, not `/logout`); `POST /internal/tenants/:id/users/:keycloak_user_id/mfa-reset` (P-34's RP-9 call, fail-closed, no reconciler).
- RP → events → O&M: `iam.tenant.events` via `tenant-orgm-q` — `TrialTenantProvisioned` (idempotent no-op reconcile on O&M's side, not the creation trigger), `TenantRealmReady` (sets `realm_id`/`keycloak_shard` — **bug fixed**: RP's frozen payload only ever carries `json:"realm"` + `json:"keycloak_shard"`, no `realm_type` field at all; O&M's consumer used to read nonexistent `realm_id`/`realm_type` keys and silently blanked both columns to empty strings on every real event — it now reads `realm` and hardcodes `realm_type='dedicated'`, since this event is only ever emitted by RP-2/RP-3, never the shared trial realm), `TenantConverted`, `DirectPaidSignup`, `TrialExpired`, `TrialReactivated`, `TenantSuspended`, `TenantOffboarded`, `TenantReactivated{source=operator}`.
- O&M → events → RP (resolves RP-4): `iam.tenant.events` `TrialStarted{tenant_id, plan, trial_ends_at}` via RP's own `tenant-realm-q`. RP tracks the trial timer locally (`tenant_realms`) and runs its own expiry sweep off it — O&M exposes no batch "expired trials" endpoint, and neither side polls the other.
- RP → O&M (pull, resolves RP-C3): `GET /internal/subscription-lapses` (I-16) — RP's subscription-lapse sweep polls this cross-tenant, BYPASSRLS-bound endpoint instead of O&M pushing a stream of cancellation-state transitions (deliberately pull, not push, since cancellation is reversible — contrast the push-only `TrialStarted` above, a one-directional transition). RP's caller sends a sentinel `x-tenant-id: 00000000-0000-0000-0000-000000000000` on this call — not because the read is tenant-scoped (it isn't; O&M's handler queries `sysPool` unconditionally and never reads the header), but because `platform-gincommon`'s shared `RequireAuth` middleware requires that header, unconditionally, on every `/api/v1` route in every IAM service, with no per-route opt-out.

`port.RealmProvisionerClient` — `CreateInvitedUser`, `DeleteUser`, `PatchRealmConfig`, `RevokeUserSessions`, `ResetMFA` (§16 OQ-8/F6). Failure maps to `503 realm_provisioner_unavailable` at invite time and at MFA-reset time (P-34, fail-closed — no reconciler for "eventually reset MFA", unlike `RevokeUserSessions`'s best-effort posture); both convergence paths idempotent and retried by reconcilers.

### 18.4 `event-consumer`

- Event Consumer → O&M: I-3 add/invite-accept; I-4 status update; I-5 soft-delete (delegate-impact gated); I-10 SAML group assertions.

### 18.5 `workflow-service` (new §8.8)

- O&M → Workflow: `GET /internal/workflows/delegate-impact?...` (query params); `POST /internal/workflows/reassign-delegate`; `POST /internal/workflows/cancel-by-delegate`.
- Workflow → events → O&M: **none** — all synchronous request/response (WFI-7).

### 18.6 `billing-service` (new §16 A10)

- Billing → events → O&M: `TenantSeatsChanged` (unconditional projection, SEAT-2/SEAT-4), on existing `billing-orgm-q`.
- Billing → O&M: `GET /internal/tenants/:id/seat-usage` (I-11, pre-check before Billing commits reduction).

Deliberately **event-driven, not synchronous** for write; O&M exposes no synchronous seat-write endpoint to Billing.

## 20. Operational Considerations

### 20.1 Outbox Health

`outbox-prune`'s batched delete at 8-day retention, daily — fully via `outbox.Runner.PrunePublished` (§13.1 above; the local `ReconcilerStore.PruneOutbox` hand-rolled SQL it replaced is now deleted, not just superseded); its test-only regression (integration tests never wired the new `OutboxRunner` dependency, so they silently exercised the no-op skip path) was found and fixed 2026-09-20 — see §13.1. Dead-letters trigger immediate page. Selective replay via `ReprocessDeadLettersWith(ctx, DLQFilter{EventType, TenantID}, limit)` (platform-events v1.4.0 DLQ API — the DLQ API itself was introduced at v1.3.0 and is unchanged at the v1.4.0 this repo now depends on).

**DLQ redrive interacts with EVT-14 recency guard:** redriven lifecycle events carry **original** old CloudEvents `time`. If newer event advanced `last_event_at`, the redrive is correctly **skipped as stale** (`iam_lifecycle_event_skipped_total++`). Post-redrive spike is expected, not an incident. If a redriven event **must** take effect, correct the projection deliberately at the source of truth. EVT-15-parked DLQ events won't redrive without a producer clock fix.

### 20.5 Workflow Service Dependency Health

Every user-removal, department-demotion/removal, resolution synchronously blocks on `WorkflowClient` (WFI-7). A Workflow outage doesn't corrupt state (WFI-8: clean `503`, no DB write) but **stops admin-initiated removals/demotions across the service** until recovery. **No cached fallback, no retry-and-defer** — caller retries. (Contrast the fail-open `DelegationCheckClient` dept-scope pre-filter in §20.7, which degrades to tenant-wide scoping rather than blocking.)

### 20.6 Seat-Limit Signal, Not an Incident

`409 seat_limit_reached` (P-6) and sustained `iam_org_membership_seat_limit_reached_total` = **expected product behavior**, not health problem. Route to CSM/Billing, not on-call. **Exception:** `seat_limit_reached` for a tenant whose `licensed_seats` should have increased via a recent `TenantSeatsChanged` — check `billing-orgm-q` consumer lag + `processed_events` for expected event ID (an integration incident).

### 20.7 Synchronous Cross-Service Dependency & Degradation Matrix (§16 A35)

For write operations, O&M availability = O&M × dependency (except fail-open). **Reads (I-8 hot path, list endpoints) have NO synchronous cross-service dependency** — Postgres+Valkey only — so authN/authZ stays available even when every write dependency is down. **None of the three ADR-0007/ADR-0008 dependencies below (Catalog, Group Mapping, Delegation) sit on the I-8 hot read path** — they fire only on admin writes, group-assertion login, and admin department-membership changes, all with far larger latency budgets than I-8's 15/30 ms SLO.

| Operation | Sync dependency | Posture | On failure | Ref |
|---|---|---|---|---|
| Invite (P-6) | RP `CreateInvitedUser` | **fail-closed** | `503 realm_provisioner_unavailable`, no invitation, retryable | §8.10, A11 |
| User removal / dept demotion·removal (P-8/I-5/P-10/P-11) | Workflow `GetDelegateImpact` | **fail-closed** | `503 workflow_service_unavailable`, no change (WFI-8) | §8.8/§8.8.4 |
| Removal resolution (P-26) | Workflow reassign/cancel + re-check | **fail-closed** | `503`, no DB write | §8.8.3 |
| Suspension (P-7) | Workflow `GetDelegateImpact` (advisory) | **fail-open** | suspend commits, advisory omitted | §8.8.5, C3 |
| `local_accounts_enabled` change (P-2) | RP `PatchRealmConfig` | **fail-open + durable reconcile** | commits, `realm_sync_pending`, 202, reconciler converges | §4.2, A7 |
| Invite compensation / revoke / expiry KC-cleanup | RP `DeleteUser` | **async + durable reconcile** | `kc_cleanup_pending`, reconciler converges (PI-9) | §13.1, A34 |
| Plan-defaults / department validity (I-8 `om:plans`/`om:departments` miss; dept-activation writes) | Catalog `GET /internal/plans`\|`/internal/departments` | **fail-closed** (not fail-open) | `503 catalog_unavailable` when cache + live call both fail | §11 (ADR-0007 Wave 1) |
| SAML/OIDC JIT group resolution (I-10) | Group Mapping `POST /internal/tenants/:id/group-resolution` | **fail-open** | cold-cache-plus-failure serves an empty resolution (login still succeeds); `group_mapping_unavailable` is declared but never actually returned (ADR-0007 Action Item 4) | §11 (ADR-0007 Wave 2) |
| Dept-scope delegate pre-filter (admin dept Assign/Remove, §8.8.4) | Delegation `GET /internal/delegations/dept-delegate` | **fail-open** | degrades to tenant-wide delegate-impact scoping — never blocks the Assign/Remove | §11 (ADR-0008) |
| Service-account-not-grantable check (Invite P-6/I-3, role-grant P-10/P-28) | Token Service `GET /service-accounts?principal_sub=` (TS-5) | **fail-open** | degrades to allowing the operation — the structural composite-FK bar (TR-8/DM-4) remains the primary guarantee | AUTH-9/IB-3 |

Three fail-closed calls on the classic write paths (invite, removal, removal-resolution) plus one new one (Catalog) stop completing during the respective dependency's outage; none corrupts state. Fail-open + durable reconcile / fail-open degrade chosen to keep security-critical, high-value, or login-critical paths available or eventually-consistent.

## 21. Performance

- **§21.1 Hot path** — Cache hit < 1 ms (Valkey GET + deserialize). Cache miss < 30 ms: single 4-table join covered by partial indexes. PgBouncer tx pooling: 4 replicas × 15 conns = 60 concurrent DB slots → ~4000 RPS at 15 ms avg, well above 500 RPS SLO.
- **§21.2 List endpoints** — P-4 keyset-paginated. `ORDER BY created_at, id LIMIT $limit + 1` (fetch-ahead row = `next_cursor`), index-covered by `idx_tm_tenant_created (tenant_id, created_at, id) WHERE deleted_at IS NULL`. Seek cost constant regardless of page depth.
- **§21.3 Group-mapping JIT** — the `group_dept_role_mappings`/`group_tenant_role_mappings`/`group_dept_mappings` tables (and their `idx_gdm_group`-style indexes) no longer live in this database (moved to Group Mapping Service, ADR-0007 Wave 2). I-10 resolves via `POST /internal/tenants/:id/group-resolution` and caches the result as `om:grm`/`om:gdm`/`om:gtrm` (600 s) with a `:stale` 24 h fallback — not a local index scan.
- **§21.5 Seat-cap count** — SEAT-1's `SELECT count(*) FROM tenant_memberships WHERE tenant_id=$1 AND deleted_at IS NULL AND status='active'` covered by the pre-existing `idx_tm_status (tenant_id, status) WHERE deleted_at IS NULL`. No new index needed for A10.
