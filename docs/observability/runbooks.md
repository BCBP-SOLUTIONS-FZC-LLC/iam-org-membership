# Runbooks: iam-org-membership alerts

Runbooks for every alert in [`deploy/monitoring/app-alerts.yml`](../../deploy/monitoring/app-alerts.yml), [`deploy/monitoring/slo-rules.yml`](../../deploy/monitoring/slo-rules.yml) and the Helm [`PrometheusRule`](../../deploy/helm/templates/prometheusrule.yaml). The heading is the alert name, and the alert's `runbook` annotation points to the same entry. Every alert carries `domain="iam"`, `service="iam-org-membership"` and `environment`. Platform-level alerts on the libraries' own metrics (HTTP error budget, message failure ratio) are documented in platform-gincommon's `docs/observability/runbooks.md`.

Common starting points:

- Rollouts: `count by (version) (platform_build_info{service="iam-org-membership",environment="$environment"})`
- Logs (Loki): `{service="iam-org-membership"} | json`, then follow `trace_id` into Tempo.
- Readiness: `GET /readyz` reports Postgres (app pool health and sysPool), Valkey and the outbox runner.

## Availability

### IAMOrgMembershipDown

**Meaning.** No pod has been scraped as healthy for 2 minutes. Every authenticated request in the platform depends on I-8, so AuthZ Enrichment is degraded.

**Triage.**
1. `kubectl get pods -l app.kubernetes.io/name=iam-org-membership` and `kubectl describe pod` for crash loops and probe failures.
2. Startup panics are deliberate for bad configuration: a missing `MIGRATION_DATABASE_URL` with `PG_BOUNCER_MODE=true`, an invalid `APP_ENV`, a queue without a RedrivePolicy (`requireDLQ`), an unregistered Glue schema definition. The panic message names it.
3. Migrations run at startup: check the logs for a migration lock wait or a dirty migration.

**Mitigate.** Roll back the last deploy, or fix the named configuration.

### IAMOrgMembershipReplicasMissing

**Meaning.** Fewer than 2 replicas are healthy (HPA `minReplicas: 2`). One more disruption takes the service down, and node drains are blocked by the PDB.

**Triage.** As for `IAMOrgMembershipDown`, per pod. Check node pressure and pending pods.

## HTTP errors and latency

### IAMOrgMembershipHighErrorRate / IAMOrgMembershipCriticalErrorRate

**Meaning.** More than 5% (warning) or 20% (critical) of requests returned 5xx over 5 minutes.

**Triage.**
1. Which routes and codes: `sum by (route, error_class) (rate(platform_http_requests_total{service="iam-org-membership",status_class="5xx"}[5m]))`.
2. The error body's `code` tells the cause:
   - `db_unavailable`: Postgres or PgBouncer. Check `platform_db_pool_connections`, `platform_db_pool_empty_acquires_total`, and the RDS and PgBouncer health.
   - `request_timeout`: a 30s request deadline, `statement_timeout` or `lock_timeout`. Check `platform_db_slow_queries_total` and slow-query logs.
   - `catalog_unavailable`: Catalog Service is down and the `om:plans` / `om:departments` cache is cold. This dependency is not fail-open.
   - `realm_provisioner_unavailable` or `workflow_service_unavailable`: these fail closed on their paths.
   - `internal_error`: an unhandled error, logged as `unhandled 500 error`.
3. Request timeouts appear on `platform_http_request_timeouts_total` (body `{"error":"request timeout"}`).
4. Dependencies: `sum by (target_service, endpoint, reason) (rate(iam_org_membership_dependency_call_failures_total[5m]))`.

**Mitigate.** Roll back a bad version, or fail over or restore the failing dependency.

### IAMOrgMembershipI8HotPathLatency / IAMSLO1_I8LatencyFastBurn / IAMSLO1_I8LatencySlowBurn / IAMSLO1b_CacheHitRateLow

**Meaning.** `GET /api/v1/internal/users/:id/memberships` (I-8) is missing its target: 15 ms on a cache hit, 30 ms on a miss. I-8 is on every authenticated request in the platform.

**Triage.** See [`../runbook-i8-latency.md`](../runbook-i8-latency.md). In short:
1. Cache: a falling `iam:i8_latency_hit_sli:ratio_rate_1h` means Valkey misses or latency (`om:memberships`).
2. Query: the four-table join (`tenant_memberships ⋈ tenants ⋈ tenant_roles ⋈ dept_memberships`). Check its plan and `platform_db_pool_acquire_duration_seconds` for pool waits.
3. Catalog: a cold `om:plans` cache adds a Catalog Service call to the miss path.

### IAMOrgMembershipHighLatency / IAMOrgMembershipCriticalLatency

**Meaning.** p99 across all routes is above 2s (warning) or 25s (critical). At 25s, requests are close to the 30s deadline and many are timing out.

**Triage.** `histogram_quantile(0.99, sum by (le, route) (rate(platform_http_request_duration_seconds_bucket{service="iam-org-membership"}[5m])))` to find the route. Then check:
- slow queries (`PG_SLOW_QUERY_THRESHOLD` warnings, `platform_db_slow_queries_total`);
- pool saturation;
- outbound calls (`iam_org_membership_dependency_call_duration_seconds` by `target_service`).

## Events and consumers

### IAMFutureLifecycleEventRejected

**Meaning.** EVT-15: an inbound lifecycle event's `time` is more than `MAX_LIFECYCLE_EVENT_SKEW_SECONDS` (300s) in the future. It was forwarded to the queue's DLQ without being applied, because applying it would freeze the EVT-14 recency guard for that tenant.

**Triage.**
1. `sum by (event_type) (rate(iam_org_membership_consumed_event_rejected_total{reason="future_time_clamp"}[15m]))`.
2. Inspect a DLQ copy (`DLQReason=future_time_clamp`, `OriginalQueue`). The envelope `source` names the producer.
3. Check that producer's clock (NTP).

**Mitigate.** Fix the clock, then redrive the DLQ. The events are reprocessed once their time is no longer in the future. See [`../runbook-schema-registry.md`](../runbook-schema-registry.md) (DLQ section).

### IAMConsumedEventSchemaViolation

**Meaning.** EVT-17: consumed `$event_type` payloads fail this service's embedded consumed-event schema. They are forwarded to the DLQ on first delivery (`DLQReason=schema_violation`).

**Triage.** Compare a DLQ copy with `api/asyncapi.yaml` and `internal/adapter/outbound/eventbus/schemas/`. Either the producer broke its contract, or this repo's copy of the consumed schema is stale.

**Mitigate.** Fix the wrong side, then redrive. See [`../runbook-schema-registry.md`](../runbook-schema-registry.md).

### IAMMalformedInboundMessage

**Meaning.** platform-events could not parse an inbound body: `reason="malformed"` (not a valid envelope) or `decode_error` (codec failure). It forwarded the original message to the DLQ instead of dropping it.

**Triage.** The library logs the message ID and `body_sha256`, never the body. Inspect the DLQ copy, whose `DLQReason`, `OriginalQueue` and `FailedAt` attributes say where it came from, and identify the producer.

**Mitigate.** Fix the producer, then redrive.

### IAMOrgMembershipConsumerDead

**Meaning.** `platform_messages_received_total` has not moved for 10 minutes on a queue. The SQS consumer may have stopped, so tenant lifecycle and billing events are not being applied and `tenants.status` drifts.

**Triage.**
1. Pod logs: `SQS consumer stopped` or `SQS consumer panic — consumer stopped, pod restart required`. The consumer heartbeat then takes the pod out of readiness.
2. `platform_dependency_request_seconds_count{dependency="sqs",outcome="error"}` and IAM permissions (`sqs:ReceiveMessage`, `GetQueueUrl`, `GetQueueAttributes`).
3. Make sure the queue isn't just idle. Off-peak silence on `billing-orgm-q` is normal, so compare with the producer's publish rate.

**Mitigate.** Restart the pod if the consumer does not recover, then fix the root cause.

### IAMStaleLifecycleEventSkippedElevated / IAMStaleLifecycleEventSkipped

**Meaning.** EVT-14: events older than the tenant's last applied event (`tenants.last_event_at`) are being skipped, still recorded in `processed_events`. Occasional skips are normal under producer reordering. A sustained rate means consumer lag or replays.

**Triage.** `sum by (event_type) (rate(iam_lifecycle_event_skipped_total[10m]))`. Then check `histogram_quantile(0.99, sum by (le) (rate(iam_lifecycle_event_lag_seconds_bucket[10m])))` (SLO-3), `platform_messages_in_flight` and the SQS age of the oldest message.

### IAMUnknownEventType

**Meaning.** A producer emits an event type this consumer has no handler for. It is acknowledged, logged and recorded in `processed_events` (forward compatibility), so nothing is lost but nothing is applied.

**Triage.** `sum by (event_type) (rate(iam_org_membership_unknown_event_acknowledged_total[1h]))`.

**Mitigate.** If the type matters to this service, add a case to `classify()` in `internal/adapter/inbound/consumer/membership_event_consumer.go` and a consumed schema. Otherwise, narrow the SNS subscription filter.

### IAMOrgMembershipOutboxDeadLetters

**Meaning.** One or more outbox events exhausted `OUTBOX_MAX_ATTEMPTS` (5) and were moved to `outbox_dead_letters`. Downstream services (AuthZ Enrichment, Delegation, Tender ACL, Workflow, Audit Log) never received them.

**Triage.**
1. `SELECT id, event_type, tenant_id, attempts, last_error, failed_at FROM outbox_dead_letters ORDER BY failed_at DESC LIMIT 20;`
2. `last_error` usually names the cause: SNS permission, topic ARN (`SNS_TOPIC_MEMBERSHIP_ARN` / `SNS_TOPIC_TENANT_ARN`), throttling, or a Glue schema version not `AVAILABLE`.
3. Events with a subject are ordered per `<tenant>/<subject>`. While a head record kept failing, later events for the same key waited (`platform_outbox_ordering_blocked_events`).

**Mitigate.** Fix the cause, then replay with the library's `outbox.Runner.ReprocessDeadLetters` / `ReprocessDeadLettersWith`. A replayed dead letter rejoins the back of its ordering key. See [`../runbook-outbox-dead-letter.md`](../runbook-outbox-dead-letter.md).

## Outbox, queues and database pools

These alerts query platform-library metrics that are still `proposed` in the registry (labelled `metric_status: proposed`).

### IAMOrgMembershipOutboxOrderingBlocked

**Meaning.** Outbox records with an ordering key (`<tenant_id>/<subject>`, written with `outbox.EnqueueOrdered`) are waiting behind an earlier record for the same key that keeps failing to publish. They are released when that head record publishes or is dead-lettered after `OUTBOX_MAX_ATTEMPTS`.

**Triage.** Outbox runner logs for the failing head (`last_error`); `platform_outbox_ordering_blocked_events`; `SELECT id, event_type, attempts, last_error FROM outbox_events WHERE published_at IS NULL AND ordering_key IS NOT NULL ORDER BY created_at LIMIT 20;` (read-only).

**Mitigate.** Fix the cause (schema version not `AVAILABLE`, SNS permission). A head that can never publish dead-letters after `OUTBOX_MAX_ATTEMPTS`, which releases its key.

### IAMOrgMembershipOutboxBacklog

**Meaning.** The oldest due outbox event has waited more than 5 minutes. Transient SNS failures (throttling, outage) do not count an attempt, so they never dead-letter — they show up here instead.

**Triage.** SNS errors in the outbox runner logs; `platform_outbox_pending_events`; whether the runner is running at all (`/readyz` reports it).

### IAMOrgMembershipDLQBacklog

**Meaning.** Messages are sitting in an inbound queue's DLQ (`platform_dlq_depth`, sampled every `SQS_QUEUE_DEPTH_INTERVAL`). The inflow alerts (`IAMFutureLifecycleEventRejected`, `IAMConsumedEventSchemaViolation`, `IAMMalformedInboundMessage`) say why they arrived; this one says they are still there.

**Mitigate.** Inspect the `DLQReason` attribute, fix the cause, then redrive. Never discard without the owning team's sign-off.

### IAMOrgMembershipDBPoolSaturated

**Meaning.** More than 90% of a pool's connections have been checked out for 10 minutes on the busiest replica. `pool="default"` is the API pool, `pool="system"` the BYPASSRLS pool (reconciler sweeps, gauge exporters, I-16, PUB-1).

**Triage.** `platform_db_pool_acquire_duration_seconds` (are callers waiting?), slow queries, long transactions; whether traffic or HPA scale-out (more replicas × `PG_MAX_CONNS` against PgBouncer) changed.

### IAMOrgMembershipDBAcquireErrors

**Meaning.** More than 5% of connection acquires fail on the worst replica (timeouts, a closed pool, or Postgres/PgBouncer unreachable). Requests answer 503 `db_unavailable`.

**Triage.** PgBouncer and RDS health; `platform_db_pool_connections{state}`; pod logs for connection errors.

### IAMOrgMembershipDBRetriesExhausted

**Meaning.** Transactions kept hitting deadlocks or serialization failures through every pgcommon retry; callers got 409 `conflict` (`concurrent_update`).

**Triage.** `reason` and `pool`; Postgres deadlock logs. Two transactions locking the same ordering keys in opposite order (the outbox advisory lock) or rows in opposite order are the usual cause.

## Security

### IAMCrossTenantRLSViolation / IAMOrgMembershipRLSCrossTenantAccess

**Meaning.** RLS-3: a query read or wrote a row whose `tenant_id` did not match the transaction's `app.tenant_id` GUC. The log is sampled, so a sampled hit means more occurred. This is either an attack or a serious application bug.

**Triage.** See [`../runbook-rls-cross-tenant.md`](../runbook-rls-cross-tenant.md). Query `rls_violation_log` (sysPool / migrator role) for `violation_type='cross_tenant_access'`. The row carries `table_name`, `row_tenant_id`, `app_tenant_id` and `query_text`.

**Mitigate.** Page security. Identify the code path from `query_text` and the trace, and roll back if a recent change introduced it.

### IAMOrgMembershipRLSMissingGUC

**Meaning.** Queries ran without a valid `app.tenant_id` GUC (`violation_type="missing_or_invalid_guc"`). RLS returns zero rows rather than leaking data, but the request silently sees nothing.

**Triage.** `query_text` in `rls_violation_log` identifies the path. Typical causes are a repository call outside `GUCBridgeMiddleware`, or an internal path that is missing `requestctx.WithSystemTenant` / `WithTenant`.

### IAMOrgMembershipTenantHeaderPathMismatch

**Meaning.** Info, not a page. An in-mesh caller sent an `/api/v1/internal/tenants/:id/*` request whose `x-tenant-id` names a different tenant than the path (IAPI-6). The path tenant is authoritative, so in the default mode the request was served for the path tenant (`action="overridden"`); nothing leaked and the answer was correct. With `STRICT_TENANT_PATH_VALIDATION=true` the same request is rejected with `400 TENANT_MISMATCH` (`action="rejected"`), so the caller is now failing.

**Triage.** `sum by (route, method, action) (rate(iam_org_membership_tenant_header_path_mismatch_total[15m]))` names the route. The WARN log `tenant_header_path_mismatch` carries `request_id`, `caller_user_id`, `caller_roles`, `path_tenant_id` and `header_tenant_id`; the roles and `request_id` identify the calling service (`commercial-system` = Billing/Entitlements; `iam-system` = an IAM service — match `request_id` in its logs). A `header_tenant_id` of `00000000-0000-0000-0000-000000000000` is a caller sending the cross-tenant sentinel to a tenant route.

**Mitigate.** Fix the caller to send `x-tenant-id` equal to the path tenant. Do not enable strict mode in an environment while this alert is firing there. If strict mode was enabled and a caller is now failing, set `iam.strictTenantPathValidation: false` (config-only rollback) and fix the caller first.

### IAMSessionRevokeFailedSustained

**Meaning.** AUTH-8: Realm Provisioner `RevokeUserSessions` calls are failing at more than 0.1/s. The privilege reduction (suspend, remove, de-privilege) is committed and enforced by this service, but the user's live Keycloak sessions survive until the backstop expires (5-minute access token plus 300s membership cache).

**Triage.** `sum by (reason) (rate(iam_auth_session_revoke_failed_total[5m]))`:
- `transport`: network or Realm Provisioner down;
- `http_5xx`: a Realm Provisioner fault;
- `http_4xx`: a contract or authorization mismatch;
- `http_other`: an unexpected status.

**Mitigate.** Restore Realm Provisioner. No replay is needed: the TTL backstop has already closed the window for past events.

## Tenant state and reconcilers

### IAMTenantOwnerless

**Meaning.** TM-12/T-13: a tenant has no active `tenant_owner` (`ownerless_since` is set). Only a platform operator can recover it.

**Mitigate.** `POST /api/v1/operator/tenants/:id/reassign-owner` (O-7) with an existing active member as the new owner.

### IAMRealmSyncPendingSustained / IAMRealmSyncPending

**Meaning.** T-15: `local_accounts_enabled` changed locally, but Realm Provisioner has not acknowledged `PatchRealmConfig`, so `realm_sync_pending` stays set.

**Triage.**
1. Check the `realm-config-sync` CronJob's last run.
2. Check `iam_org_membership_realm_sync_failed_total{stage}` in the Pushgateway (`cronjob="realm-config-sync"`).
3. Check Realm Provisioner health.

**Mitigate.** Restore Realm Provisioner. The CronJob converges on its next run.

### IAMPendingInvitationsStale

**Meaning.** Pending invitations past `expires_at` have not been flipped to `expired`, so the `invitation-expiry` CronJob is probably not running.

**Triage.** The CronJob's last successful run and pod logs (`kubectl get jobs -l app.kubernetes.io/component=reconciler`).

### IAMSeatOverageActive / IAMSeatOverageActiveApproachingGrace

**Meaning.** SEAT-5: tenants have been over their licensed seats (`overage_since` set) for more than 24h. The Helm rule fires 5 days before the `SEAT_OVERAGE_GRACE_DAYS` (30) window closes. This is a business signal for Billing, not a fault.

**Mitigate.** Billing contacts the tenant. Enforcement follows SEAT-4 at the end of the grace window.

### IAMSLO3_ReconcilerMarkersElevated

**Meaning.** The combined durable markers (`iam:reconciler_markers:sum`: ownerless + realm-sync-pending + seat-overage + stale-invitation gauges) have stayed above 5 for 20 minutes. Some reconciler is not converging.

**Triage.** Check which gauge is high, then that reconciler's CronJob (`realm-config-sync`, `invitation-expiry`, `seat-overage-reconcile`) or the O-7 operator action for ownerless tenants.

## Write availability

### IAMSLO2_WriteErrorRateFastBurn

**Meaning.** More than 1.44% of mutating requests (`POST|PUT|PATCH|DELETE`) returned 5xx over 5 minutes, which is 14.4× the 99.9% budget.

**Triage.** As for `IAMOrgMembershipHighErrorRate`, restricted to write routes. Check whether the error is `db_unavailable` or a fail-closed dependency (`realm_provisioner_unavailable`, `catalog_unavailable`), and check `/readyz`.

## Audit delivery

### IAMAuditDeliveryFailed

**Meaning.** `iam_org_membership_audit_delivery_failed_total{entry_type}` is nonzero over 10 minutes. Best-effort AL-5 direct-write goroutines in `TenantService.auditSettingChange` or `InvitationService.auditInviteEvent` are returning errors. P-2 and invite flows are unaffected (fail-open), but the listed `entry_type` audit records are silently absent from iam-audit-log.

**Triage.**
1. Check `AUDIT_LOG_BASE_URL` is set — if blank, all audit writes are no-ops by design.
2. Check iam-audit-log's own health (`/healthz`, its own alerts) — an outage there causes all AL-5 calls to fail.
3. Check for 429 or 5xx from iam-audit-log in this service's structured logs (`grep 'auditlog' <pod-logs>`).
4. Verify the idempotency key pattern — a restart may cause a burst of retry-eligible calls with the same key (harmless, idempotent).

**Resolve.** Fix the upstream iam-audit-log issue or set `AUDIT_LOG_BASE_URL`. The fire-and-forget goroutines do not retry, so missed entries are permanent once the error passes — reconcile via the Audit Log's own backfill mechanism if compliance requires it.
