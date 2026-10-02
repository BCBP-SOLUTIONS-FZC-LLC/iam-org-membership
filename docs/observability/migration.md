# Renamed and removed metric names

iam-org-membership moved to the Enterprise Platform Observability Standard in three steps. The service has never been deployed to any environment, so no dashboard, alert or stored series depended on the old names. Every old name was **renamed or removed outright, with no compatibility period**, and the alerts, recording rules, SLOs, HPA rule and dashboards in this repo were rewritten in the same change. `metricslint refs` (in CI) rejects any removed `platform_*`/library name that comes back.

## 1. Three-tier hierarchy (2026-09-11)

Service metrics moved from a flat `iam_*` namespace to the three tiers: names unique to this service gained the `iam_org_membership_` prefix, and IAM-wide concepts kept `iam_*`.

| Removed name | Replacement | Notes |
|---|---|---|
| `iam_unknown_event_acknowledged_total` | `iam_org_membership_unknown_event_acknowledged_total` | |
| `iam_delegate_suspend_impact_total` | `iam_org_membership_delegate_suspend_impact_total` | |
| `iam_tenant_ownerless_escalated_total` | `iam_org_membership_tenant_ownerless_escalated_total` | |
| `iam_seat_overage_started_total` | `iam_org_membership_seat_overage_started_total` | |
| `iam_seat_limit_reached_total` | `iam_org_membership_seat_limit_reached_total` | |
| `iam_invite_throttled_total` | `iam_org_membership_invite_throttled_total` | |
| `iam_realm_sync_failed_total` | `iam_org_membership_realm_sync_failed_total` | |
| `iam_delegate_removal_blocked_total` | `iam_org_membership_delegate_removal_blocked_total` | |
| `iam_delegate_reassignment_total` | `iam_org_membership_delegate_reassignment_total` | |
| `iam_membership_exists_check_total` | `iam_org_membership_membership_exists_check_total` | |
| `iam_processed_events_duplicates_total` | `iam_org_membership_processed_events_duplicates_total` | Tier 3 predecessor of `platform_duplicate_messages_total` |
| `iam_tenant_ownerless` | `iam_org_membership_tenant_ownerless` | gauge |
| `iam_realm_sync_pending` | `iam_org_membership_realm_sync_pending` | gauge |
| `iam_seat_overage_active` | `iam_org_membership_seat_overage_active` | gauge |
| `iam_pending_invitations_stale` | `iam_org_membership_pending_invitations_stale` | gauge |
| `iam_xsvc_call_latency_seconds` | `iam_org_membership_dependency_call_duration_seconds` | Tier 3 predecessor of `platform_dependency_request_seconds` |
| `iam_xsvc_call_errors_total` | `iam_org_membership_dependency_call_failures_total` | label renamed again in step 3 |
| `iam_session_revoke_failed_total` | `iam_auth_session_revoke_failed_total` | Tier 2 |
| `iam_stale_lifecycle_event_skipped_total` | `iam_lifecycle_event_skipped_total` | Tier 2 |
| `iam_lifecycle_consumer_lag_seconds` | `iam_lifecycle_event_lag_seconds` | Tier 2 |
| `iam_future_lifecycle_event_rejected_total` | `iam_org_membership_consumed_event_rejected_total{reason="future_time_clamp"}` | via `platform_dlq_messages_total{reason}` in between; see step 3 |

## 2. Platform library migration (2026-10-02)

The platform libraries dropped their pre-standard names (gincommon v1.4.0, platform-events/v2, platform-pgcommon/v2). Every query in this repo was rewritten to the `platform_*` successor:

| Removed name | Replacement | Label changes |
|---|---|---|
| `http_requests_total` | `platform_http_requests_total` | + `domain`, `environment`; − `version` |
| `http_request_duration_seconds` | `platform_http_request_duration_seconds` | same; identical buckets |
| `http_requests_per_second` (prometheus-adapter) | `platform_http_requests_per_second` | derived from `platform_http_requests_total` |
| `outbox_dead_letters_total` | `platform_dlq_messages_total{operation="outbox_publish"}` | `reason="max_attempts"` |

The identity became mandatory at the same time: `domain="iam"` via `gincommon.Config.Domain` / `OBSERVABILITY_DOMAIN`, and `APP_ENV` restricted to `local, dev, test, staging, prod` (`production` is rejected). `platform-pgcommon`'s `pgcommon_*` and platform-events' `events_*` / `outbox_*` / `sqs_*` names were removed the same way; their replacements are in each library's `docs/observability/`.

## 3. Label and ownership corrections (LLD rev 2.15–2.16, 2026-10-02/03)

| Before | After | Why |
|---|---|---|
| `platform_dlq_messages_total{reason="future_time_clamp"\|"schema_violation"}` recorded by this service | `iam_org_membership_consumed_event_rejected_total{event_type,reason}`; `platform_dlq_messages_total` counted only by platform-events (`reason="explicit"` for these forwards) | Every reject was counted twice: once by the service, once by the library's forward |
| `iam_org_membership_dependency_call_failures_total{outcome="5xx"\|"timeout"\|"fallback_served"}` | `{reason="timeout"\|"transport"\|"http_4xx"\|"http_5xx"\|"http_other"\|"fallback_served"}` | `outcome` is a registry label with the fixed set `success, failure, error`; the old values also counted 4xx and transport errors as `5xx` |
| `iam_auth_session_revoke_failed_total{reason="503"}` (raw status code) | `{reason="http_4xx"\|"http_5xx"\|"http_other"}`, plus `transport` | The registry's `reason` values must start with a letter |
| `platform_duplicate_messages_total` recorded by this service (queue = logical name) | recorded by platform-events' inbox (queue from the queue URL) | Dedup moved onto `pkg/inbox`, which counts it itself |
| `platform_dependency_errors_total` | not emitted | Never part of the registry; failures are on the Tier 3 counter, and `platform_dependency_request_seconds_count{outcome="error"}` is the registry concept |
| Tier 2 `iam_*` collectors defined by hand | built from the registry (`gincommon.NewShared*Vec`, platform-gincommon v1.5.0) | The registry now defines their help, labels and buckets; `iam_rls_violations_total{violation_type}` is a fixed set (`other` for anything unexpected) |

## Querying after the migration

```promql
# Permanent rejects by reason (was platform_dlq_messages_total{reason=...})
sum by (environment, reason) (rate(iam_org_membership_consumed_event_rejected_total[5m]))

# DLQ inflow from every cause, counted once (library)
sum by (environment, operation, reason) (increase(platform_dlq_messages_total{service="iam-org-membership"}[15m]))

# Dependency failures excluding the stale-if-error fallback
sum by (target_service, reason) (rate(iam_org_membership_dependency_call_failures_total{reason!="fallback_served"}[5m]))
```
