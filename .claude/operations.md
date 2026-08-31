# Operations

## 10. Security

### 10.1 Tenant Isolation — Three Layers

- **Layer 1** — Keycloak realm boundary.
- **Layer 2** — PostgreSQL RLS with `app.tenant_id` GUC, `FORCE ROW LEVEL SECURITY`, `WITH CHECK`. `tenants` policy uses `id = current_setting('app.tenant_id')::uuid` (single-row visibility).
- **Layer 3** — Audit-tagged cross-tenant detection via `rls_violation_log` + CloudWatch alarms.

### 10.2 Network Isolation

- `/api/v1/internal/*` — Kubernetes NetworkPolicy; only IAM-namespace service accounts reach it; public Envoy does not route.
- `/api/v1/operator/*` — Same network-layer isolation (§16 C1, AUTH-7): served **only** via operator ingress; NetworkPolicy blocks from tenant-facing network. Even a spoofed `x-tenant-roles: platform_operator` **cannot reach an operator route from public network**.

### 10.3 Input Validation

`slug` — `^[a-z0-9][a-z0-9-]{2,62}[a-z0-9]$`, immutable after set. `default_locale` — BCP-47. `role_level`/`role_code` — ENUM. `scope` — ENUM. `keycloak_group_name` — ≤200 chars, `^[a-zA-Z0-9_./-]+$`. All UUIDs validated at handler layer.

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
- **SLO-3** — Inbound projection freshness explicitly alerted. Measured via `iam_lifecycle_consumer_lag_seconds`. **Primary drift signal** because EVT-14 skips stale events **silently** — lag alert is the drift signal, not DLQ.

### 11.2 Prometheus Metrics

All `iam_`-prefixed. Cardinality-bounded: `tenant_id` labels capped by tenant count (~1500–2000 target); other labels are small enum fan-outs.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `iam_membership_joins_total` | Counter | `tenant_id`, `source` | Members added |
| `iam_membership_leaves_total` | Counter | `tenant_id`, `reason` | Members removed |
| `iam_memberships_cache_hit_ratio` | Gauge | — | Valkey hit rate for `om:memberships:*` |
| `iam_membership_lookup_latency_seconds` | Histogram | `result (hit\|miss)` | I-8 latency; source for SLO-1 |
| `iam_processed_events_duplicates_total` | Counter | `consumer` | Duplicates skipped (IDEMP-2) |
| `iam_lifecycle_consumer_lag_seconds` | Gauge | `queue` | SQS `ApproximateAgeOfOldestMessage` — SLO-3 primary drift signal |
| `iam_xsvc_call_latency_seconds` | Histogram | `service` (`catalog`\|`group_mapping`\|`delegation`), `endpoint` | Latency of the three synchronous cross-service client calls added by the ADR-0007/ADR-0008 decomposition |
| `iam_xsvc_call_errors_total` | Counter | `service`, `endpoint`, `outcome` (`5xx`\|`timeout`\|`fallback_served`) | Cross-service call failures; `fallback_served` recorded by the calling `CatalogService`/`GroupMappingService`, not the client |
| `iam_membership_exists_check_total` | Counter | `caller`, `result` (`active`\|`inactive`) | I-15 grant-time membership-existence checks served (Delegation Service, Tender ACL Service) |
| `outbox_dead_letters_total` | Counter | `event_type` | Dead letters |
| `iam_delegate_removal_blocked_total` | Counter | `tenant_id`, `trigger (full_removal\|dept_demotion\|dept_removal)` | `409 workflow_resolution_required` |
| `iam_delegate_reassignment_total` | Counter | `tenant_id` | Successful `replace_delegate` |
| `iam_delegate_workflow_cancel_total` | Counter | `tenant_id` | Successful `stop_workflows` |
| `iam_delegate_suspend_impact_total` | Counter | `tenant_id` | P-7 suspend advisory fired (§8.8.5, not a block) |
| `iam_session_revoke_failed_total` | Counter | `tenant_id`, `trigger (suspend\|removal\|deprivilege)` | AUTH-8 RP call failed — sustained rate pages |
| `iam_seat_limit_reached_total` | Counter | `tenant_id` | P-6 refused (SEAT-1) |
| `iam_seat_overage_tenants` | Gauge | — | `count(*) WHERE overage_since IS NOT NULL` |
| `iam_seat_overage_started_total` | Counter | `tenant_id` | `overage_since` stamped |
| `iam_invitations_created_total` / `_accepted_total` / `_expired_total` / `_revoked_total` | Counter | `tenant_id` | Invitation lifecycle |
| `iam_invite_throttled_total` | Counter | `tenant_id`, `reason (cooldown\|rate_limit)` | Pre-RP-call throttle (PI-11/PI-12) |
| `iam_pending_invitations` | Gauge | `tenant_id` | Current unexpired pending count |
| `iam_stale_lifecycle_event_skipped_total` | Counter | `event_type` | EVT-14 stale skip (post-DLQ-redrive spikes are expected) |
| `iam_future_lifecycle_event_rejected_total` | Counter | `event_type` | EVT-15 clamp — **any nonzero pages** |
| `iam_invite_kc_cleanup_pending` | Gauge | — | `count(*) WHERE kc_cleanup_pending` |
| `iam_invite_kc_cleanup_failed_total` | Counter | `tenant_id` | PI-9 reconciler RP failures |
| `iam_realm_sync_pending` | Gauge | — | `count(*) WHERE realm_sync_pending` |
| `iam_realm_sync_failed_total` | Counter | `tenant_id` | T-15 reconciler failures (security-relevant on disable) |
| `iam_tenant_ownerless_total` | Counter | `tenant_id` | TM-12 escalations set |
| `iam_tenant_ownerless` | Gauge | — | `count(*) WHERE ownerless_since IS NOT NULL` — any nonzero pages `platform_operator` |
| `iam_group_mapping_resolution_errors_total` | Counter | `tenant_id` | JIT no-match |

**Alerts:**
- `outbox_dead_letters_total rate > 0` → page
- `iam_tenant_ownerless > 0` → page `platform_operator` (O-7 required)
- `iam_future_lifecycle_event_rejected_total rate > 0` → page (producer clock skew / bad replay)
- Sustained `iam_session_revoke_failed_total` → page (AUTH-8 fast-kill degraded, only TTL-bounded)
- `iam_lifecycle_consumer_lag_seconds > 30` for ~2 min → page (SLO-3 breach, primary drift signal)
- `iam_realm_sync_pending > 0` sustained beyond ~10 min OR any un-applied disable → page (T-15)
- Sustained `iam_realm_sync_failed_total` → page
- Sustained `iam_invite_throttled_total` for one tenant → warn (email abuse / bad client)
- Sustained `iam_delegate_removal_blocked_total` without matching `_reassignment_total`/`_workflow_cancel_total` → warn (admins hitting block, not completing resolution)
- Spike in `iam_seat_limit_reached_total` for a tenant → **informational Slack to CSM/Billing** (not on-call — genuine "buy more seats" signal)
- Tenant `overage_since` older than `SEAT_OVERAGE_GRACE_DAYS` → notify Billing (enforcement owner)

**Metric naming (§16 A50/J4):** `iam_` subsystem prefix kept; emitting service disambiguated by Prometheus `job` label. Names unique across IAM (no collisions). Dashboards/alerts on shared names aggregate `by (job)`.

**Cardinality guardrails (§16 A48):** no unbounded / user-supplied label (`user_id`, `email` etc.) may be added. If tenant count grows past ~10k, highest-churn counters drop `tenant_id` in favor of structured logs / OTel exemplars; low-cardinality gauges keep it.

### 11.3 OTel Tracing

`platform-gincommon.InitTracingFromEnv()` + `platform-pgcommon.NewOTelQueryTracer`. Cross-service client spans (`catalogadmin`/`groupmappingclient`/`delegationcheck`) carry child spans for the outbound HTTP call + DB write where applicable. W3C `traceparent` propagated via `gincommon.PropagateHeaders`.

### 11.4 Structured Logs (Zap)

Slow queries > 200 ms at WARN (`tenant_id` redacted). RLS violations at ERROR (1% sampled). Delegate-impact events at INFO. `tenant_ownerless_escalation` at ERROR (durable, page-worthy record from I-5 cascade); `tenant_owner_reassigned` at INFO (O-7 clear side).

## 12. Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | (required) | Postgres DSN for `org_membership` |
| `MIGRATION_DATABASE_URL` | (required) | Direct Postgres DSN for migrations (bypasses PgBouncer, CONFIG-2) |
| `PG_BOUNCER_MODE` | `false` | `true` in prod/staging |
| `PG_MAX_CONNS` | `15` | Pool max per pod |
| `VALKEY_URL` | (required) | ElastiCache endpoint |
| `VALKEY_TIMEOUT_MS` | `50` | Cache operation timeout (miss on timeout, CONFIG-3) |
| `SNS_TOPIC_ARN_MEMBERSHIP` | (required) | `iam.membership.events` |
| `SNS_TOPIC_ARN_TENANT` | (required) | `iam.tenant.events` |
| `SQS_QUEUE_URL_TENANT_EVENTS` | (required) | `tenant-orgm-q` |
| `SQS_QUEUE_URL_BILLING_EVENTS` | (required) | `billing-orgm-q` |
| `WORKFLOW_SERVICE_BASE_URL` / `_TIMEOUT_MS` | required / 3000 | §8.8 delegate-impact/reassign/cancel |
| `REALM_PROVISIONER_BASE_URL` / `_TIMEOUT_MS` | required / 3000 | Invited-user create/delete, realm-config patch, session revoke |
| `CATALOG_ADMIN_BASE_URL` / `_TIMEOUT_MS` | required / 3000 | Catalog / Admin Config Service (ADR-0007 Wave 1) — `om:plans`/`om:departments` source. **Not fail-open**: an unconfigured/failed call with no cache surfaces `catalog_unavailable` (503) |
| `GROUP_MAPPING_BASE_URL` / `_TIMEOUT_MS` | required / 300 | Group Mapping / JIT Config Service (ADR-0007 Wave 2) — I-10 SAML group→dept/role resolution. Fails **open** (ADR-0007 Action Item 4): cold-cache-plus-failure serves an empty resolution rather than blocking login |
| `DELEGATION_BASE_URL` / `_TIMEOUT_MS` | required / 300 | Delegation Service (ADR-0008) — §8.8.4 dept-scope delegate pre-filter on admin Assign/Remove. Fails **open**: degrades to tenant-wide delegate-impact scoping, never blocks the operation. (Previously missing from `.env-example`/Helm values — a real bug, fixed in the ADR-0007/ADR-0008 decomposition pass: the client always failed to construct and every call silently degraded to tenant-wide scoping in every environment.) |
| `INVITATION_EXPIRY_DAYS` | `7` | Pending invitation window; **must equal Keycloak invite action-token lifespan** |
| `INVITE_REINVITE_COOLDOWN_MINUTES` | `60` | Per-email cooldown (PI-11); `0` disables |
| `INVITE_MAX_PER_TENANT_PER_HOUR` | `200` | Per-tenant hourly ceiling (PI-12); `0` disables |
| `SEAT_OVERAGE_GRACE_DAYS` | `30` | Drives `grace_ends_at` + past-grace alert. **Not** an auto-action trigger (SEAT-3/SEAT-4) — Billing's enforcement decision |
| `MAX_LIFECYCLE_EVENT_SKEW_SECONDS` | `300` | EVT-15 future-time clamp threshold |
| `OUTBOX_POLL_INTERVAL_MS` | `500` | Outbox poll |
| `OUTBOX_BATCH_SIZE` | `50` | Publish batch size |
| `OUTBOX_MAX_ATTEMPTS` | `5` | DLQ threshold (EVT-5) |
| `OUTBOX_DRAIN_TIMEOUT_S` | `30` | Shutdown drain |
| `OUTBOX_STARTUP_JITTER_S` | `7` | HPA scaling jitter |
| `GLUE_REGISTRY_MEMBERSHIP_NAME` | (required) | `iam-membership-events` — omit for `NoopCodec` (plain JSON) on that topic |
| `GLUE_REGISTRY_TENANT_NAME` | (required) | `iam-tenant-events` — omit for `NoopCodec` (plain JSON) on that topic |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (required) | OTLP collector |
| `BUILD_VERSION` | (required) | CI-injected |

**CONFIG-1..5:** All infra endpoints env-supplied (no compiled config, only `BUILD_VERSION`). Same image runs everywhere.

## 13. Deployment & Scaling

Helm chart mirrors `iam-user-profile`. `terminationGracePeriodSeconds = 75`. HPA: 2–8 replicas on CPU + `iam_memberships_cache_hit_ratio`. PDB `minAvailable: 1`. Resources: CPU 100m/500m, Memory 128Mi/384Mi.

### 13.1 CronJobs

| CronJob | Schedule | Purpose |
|---|---|---|
| `trial-cleanup` | `0 2 * * *` | Phase-2 DB executor: soft-delete + PII-scrub for `trial_expired` past 15-d grace (§15.3) |
| `processed-events-prune` | `0 * * * *` | Delete `processed_events > 8 days` |
| `invitation-expiry` | `*/15 * * * *` | Past-`expires_at` pending → `expired` + `kc_cleanup_pending=true` (PI-5/PI-9) |
| `invitation-kc-cleanup` | `*/10 * * * *` | Saga-compensation reconciler (PI-9): sweep `kc_cleanup_pending`, call `RealmProvisioner.DeleteUser`, clear flag |
| `seat-overage-reconcile` | `0 * * * *` | Seat-overage marker backstop (SEAT-5): recompute `overage_since`; also drives past-grace alert |
| `realm-config-sync` | `*/2 * * * *` | Realm-config reconciler (T-15): sweep `realm_sync_pending`, call idempotent `PatchRealmConfig`; **prioritises disables** (security-tightening) |
| `outbox-prune` | `0 3 * * *` | Batched raw-SQL delete at 8-day retention, capped per tick at `jctx.BatchLimit` (default 500) — not `outbox.Runner.PrunePublished` |

Exactly **7** CronJobs, dispatched via `cmd/reconciler/main.go --job=<name>` (verified against `cmd/reconciler/jobs/` and `main.go`'s `registry` map). `quota-reset` and `quota-utilization-metrics` **removed** (§16 A26 — moved to Usage & Metering). `delegation-expiry`, `delegation-review`, `delegation-cleanup` (→ Delegation Service) and `acl-cleanup` (→ Tender ACL Service) **removed** by the ADR-0007/ADR-0008 decomposition — those tables and their lifecycle no longer live in this database.

### 13.3 Migration Safety

Rolling deploy, 3 replicas. `migrate.Runner` `lock_timeout=30s`. Additive changes zero-downtime. `UNIQUE` via `CREATE UNIQUE INDEX CONCURRENTLY` + `ADD CONSTRAINT ... USING INDEX` in separate releases (MIG-8).

## 14. Testing Strategy

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

- RP → O&M: `POST /internal/tenants` (I-1); `PATCH /internal/tenants/:id` (I-2, sets realm_id/realm_type/keycloak_shard together).
- O&M → RP: `POST /internal/tenants/:id/users` (invited-user create, §16 A11); `DELETE /internal/tenants/:id/users/:keycloak_user_id` (compensating delete, PI-9 durable via `kc_cleanup_pending`); `PATCH /internal/tenants/:id/realm-config` (T-15 realm-config propagation, Option A local-first commit-then-call, durable via `realm_sync_pending`); `POST /internal/tenants/:id/users/:keycloak_user_id/logout` (AUTH-8 session revocation, best-effort/fail-open).
- RP → events → O&M: `iam.tenant.events` (TenantRealmReady, TenantConverted, TenantSuspended, TenantOffboarded, …) via `tenant-orgm-q`.
- O&M → events → RP (new, resolves RP-4): `iam.tenant.events` `TrialStarted{tenant_id, plan, trial_ends_at}` via RP's own `tenant-realm-q`. RP tracks the trial timer locally (`tenant_realms`) and runs its own expiry sweep off it — O&M exposes no batch "expired trials" endpoint, and neither side polls the other.

`port.RealmProvisionerClient` — `CreateInvitedUser`, `DeleteUser`, `PatchRealmConfig`, `RevokeUserSessions`, `ResetMFA` (new, §16 OQ-8/F6). Failure maps to `503 realm_provisioner_unavailable` at invite time and at MFA-reset time (P-34, fail-closed — no reconciler for "eventually reset MFA", unlike `RevokeUserSessions`'s best-effort posture); both convergence paths idempotent and retried by reconcilers.

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

`outbox-prune`'s batched raw-SQL delete at 8-day retention, daily (not `outbox.Runner.PrunePublished`). Dead-letters trigger immediate page. Selective replay via `ReprocessDeadLettersWith(ctx, DLQFilter{EventType, TenantID}, limit)` (platform-events v1.3.0 DLQ API).

**DLQ redrive interacts with EVT-14 recency guard:** redriven lifecycle events carry **original** old CloudEvents `time`. If newer event advanced `last_event_at`, the redrive is correctly **skipped as stale** (`iam_stale_lifecycle_event_skipped_total++`). Post-redrive spike is expected, not an incident. If a redriven event **must** take effect, correct the projection deliberately at the source of truth. EVT-15-parked DLQ events won't redrive without a producer clock fix.

### 20.5 Workflow Service Dependency Health

Every user-removal, department-demotion/removal, resolution synchronously blocks on `WorkflowClient` (WFI-7). A Workflow outage doesn't corrupt state (WFI-8: clean `503`, no DB write) but **stops admin-initiated removals/demotions across the service** until recovery. **No cached fallback, no retry-and-defer** — caller retries. (Contrast the fail-open `DelegationCheckClient` dept-scope pre-filter in §20.7, which degrades to tenant-wide scoping rather than blocking.)

### 20.6 Seat-Limit Signal, Not an Incident

`409 seat_limit_reached` (P-6) and sustained `iam_seat_limit_reached_total` = **expected product behavior**, not health problem. Route to CSM/Billing, not on-call. **Exception:** `seat_limit_reached` for a tenant whose `licensed_seats` should have increased via a recent `TenantSeatsChanged` — check `billing-orgm-q` consumer lag + `processed_events` for expected event ID (an integration incident).

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

Three fail-closed calls on the classic write paths (invite, removal, removal-resolution) plus one new one (Catalog) stop completing during the respective dependency's outage; none corrupts state. Fail-open + durable reconcile / fail-open degrade chosen to keep security-critical, high-value, or login-critical paths available or eventually-consistent.

## 21. Performance

- **§21.1 Hot path** — Cache hit < 1 ms (Valkey GET + deserialize). Cache miss < 30 ms: single 4-table join covered by partial indexes. PgBouncer tx pooling: 4 replicas × 15 conns = 60 concurrent DB slots → ~4000 RPS at 15 ms avg, well above 500 RPS SLO.
- **§21.2 List endpoints** — P-4 keyset-paginated. `ORDER BY created_at, id LIMIT $limit + 1` (fetch-ahead row = `next_cursor`), index-covered by `idx_tm_tenant_created (tenant_id, created_at, id) WHERE deleted_at IS NULL`. Seek cost constant regardless of page depth.
- **§21.3 Group-mapping JIT** — the `group_dept_role_mappings`/`group_tenant_role_mappings`/`group_dept_mappings` tables (and their `idx_gdm_group`-style indexes) no longer live in this database (moved to Group Mapping Service, ADR-0007 Wave 2). I-10 resolves via `POST /internal/tenants/:id/group-resolution` and caches the result as `om:grm`/`om:gdm`/`om:gtrm` (600 s) with a `:stale` 24 h fallback — not a local index scan.
- **§21.5 Seat-cap count** — SEAT-1's `SELECT count(*) FROM tenant_memberships WHERE tenant_id=$1 AND deleted_at IS NULL AND status='active'` covered by the pre-existing `idx_tm_status (tenant_id, status) WHERE deleted_at IS NULL`. No new index needed for A10.
