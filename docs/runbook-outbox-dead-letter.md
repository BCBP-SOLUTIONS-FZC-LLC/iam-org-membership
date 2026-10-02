# Runbook: Outbox Dead Letter

**Alert:** `IAMOrgMembershipOutboxDeadLetters` (see also [`observability/runbooks.md`](observability/runbooks.md#iamorgmembershipoutboxdeadletters))
**Severity:** Critical — domain events are being lost.

## Symptoms
- `platform_dlq_messages_total{operation="outbox_publish"}` (outbox dead-letters, `reason="max_attempts"`) rising in Prometheus
- Events not reaching downstream consumers (AuthZ Enrichment, Delegation, Tender ACL)

## Immediate Actions
1. Query (platform-events moves an exhausted record from `outbox_events` to its own `outbox_dead_letters` table): `SELECT id, event_type, tenant_id, attempts, last_error, failed_at FROM outbox_dead_letters ORDER BY failed_at DESC LIMIT 20`
2. Inspect the failed event payload and error message
3. Check Glue Schema Registry — is the schema `AVAILABLE`? Run `make schema-verify`
4. Check SNS topic ARN (`SNS_TOPIC_MEMBERSHIP_ARN`, `SNS_TOPIC_TENANT_ARN`) is set and reachable

## Root Causes
- Schema mismatch: published payload does not match the registered Glue schema
- SNS topic unreachable or wrong ARN configured
- Network partition between pod and SNS endpoint

## Resolution
- Schema mismatch: fix the payload shape or re-register the schema; retry dead-lettered rows
- Wrong ARN: fix Helm values and redeploy
- To retry dead-lettered rows, use platform-events' `outbox.Runner.ReprocessDeadLetters(ctx, limit)` (or `ReprocessDeadLettersWith` with a `DLQFilter`, e.g. one event type) — never hand-written SQL against the library's tables. A replayed record rejoins the back of its ordering key (`<tenant_id>/<subject>`).
