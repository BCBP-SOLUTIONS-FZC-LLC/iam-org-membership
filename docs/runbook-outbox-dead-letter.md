# Runbook: Outbox Dead Letter

**Alert:** `IAMOutboxDeadLetters`
**Severity:** Critical — domain events are being lost.

## Symptoms
- `outbox_dead_letters_total` rising in Prometheus
- Events not reaching downstream consumers (AuthZ Enrichment, Delegation, Tender ACL)

## Immediate Actions
1. Query: `SELECT * FROM outbox_events WHERE dead_lettered_at IS NOT NULL ORDER BY dead_lettered_at DESC LIMIT 20`
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
- To manually retry dead-lettered outbox rows: `UPDATE outbox_events SET dead_lettered_at = NULL WHERE dead_lettered_at IS NOT NULL`
