# Runbook — AWS Glue Schema Registry state (two registries)

## The contract

`iam-org-membership` reads its event schemas from **two** AWS Glue Schema
Registries at startup (`eventbus.NewGlueCodec` per registry pre-fetches every
version ID). Startup fails fast if any expected schema is missing — the pod
will not accept traffic. This is intentional; publishing an event whose header
points at a non-existent Glue schema version silently poisons every consumer
downstream.

The two-registry split mirrors the two SNS topics owned by `RoutingPublisher`
(LLD §7.3.1). Every event's registered Glue schema name is **PascalCase** and
matches the file name in `internal/adapter/outbound/eventbus/schemas/`. There
is no snake_case translation — `//go:embed schemas/*.json` uses the same
identifiers used by `schema-gov register`.

### Registry: `iam-membership-events` (env `GLUE_REGISTRY_NAME_MEMBERSHIP`)

Backs SNS topic `iam.membership.events`. Twelve schemas — all events O&M
produces on the membership topic (§7.3):

| Domain event type | Registered Glue schema name |
|---|---|
| `department.membership.granted` | `DepartmentMembershipGranted` |
| `department.membership.revoked` | `DepartmentMembershipRevoked` |
| `department.membership.level_changed` | `DepartmentMembershipLevelChanged` |
| `tenant.role.granted` | `TenantRoleGranted` |
| `tenant.role.revoked` | `TenantRoleRevoked` (§16 A14) |
| `delegation.started` | `DelegationStarted` |
| `delegation.ended` | `DelegationEnded` (extended `ended_reason` incl. `delegate_removed`, DEL-7) |
| `tender.assignee.overridden` | `TenderAssigneeOverridden` (I-13) |
| `mfa.reset` | `MFAReset` (P-34, §16 OQ-8/F6) |
| `tenant.seat.overage.started` | `TenantSeatOverageStarted` (SEAT-5) |
| `tenant.seat.overage.resolved` | `TenantSeatOverageResolved` |
| `tenant.state.changed` | `TenantStateChanged` (§16 A61, EVT-16 relay) |

### Registry: `iam-tenant-events` (env `GLUE_REGISTRY_NAME_TENANT`)

Backs SNS topic `iam.tenant.events`. Two schemas — the only tenant-lifecycle
events O&M **produces** (all other messages on that topic are Realm-Provisioner
produced and O&M **consumes** them via `tenant-orgm-q`; produce/consume sets
are disjoint per HLD §9.1.1):

| Domain event type | Registered Glue schema name |
|---|---|
| `tenant.created` | `TenantCreated` |
| `trial.started` | `TrialStarted` |

Total: **14 schemas across two registries**. The event-type ↔ schema-name
mapping lives in `domain.GlueSchemaName` (`internal/core/domain/events.go`) —
an explicit switch, not a mechanical transform, so a new event type will not
silently register under a wrong name.

## Pre-deploy checklist

Run once in the target AWS account before every promotion. Fails loudly if any
schema is missing.

```bash
export GLUE_REGISTRY_NAME_MEMBERSHIP=iam-membership-events   # or the env-specific name
export GLUE_REGISTRY_NAME_TENANT=iam-tenant-events
export AWS_REGION=ap-south-1
make schema-verify
```

`make schema-verify` wraps
`docker run ghcr.io/bcbp-solutions-fzc-llc/platform-schemagov:0.4` and runs
schema-gov against `api/asyncapi.yaml` + `internal/adapter/outbound/eventbus/schemas/*.json`.
The eight validation passes are:

1. Envelope well-formedness (CloudEvents 1.0.2).
2. Draft-07 JSON Schema validity on each `schemas/*.json`.
3. Coverage — every event type declared in `api/asyncapi.yaml` has a matching
   `schemas/*.json` file.
4. Reverse-coverage — every `schemas/*.json` corresponds to an event type in
   the AsyncAPI channels.
5. Registry-name resolution — `domain.GlueSchemaName` returns the same
   PascalCase identifier as the file stem.
6. Topic-partition — every event maps to exactly one of the two topics; no
   event straddles topics (routing keyed on `Envelope.Source`).
7. Lifecycle enforcement (`enforce-lifecycle` sub-command) — no backward-
   incompatible changes without an accompanying `additive-then-destructive`
   migration entry.
8. Registry drift — every registered schema in Glue (per registry) has a live
   `schemas/*.json`; no orphan Glue schemas.

Expected output:

```
OK: 12 schemas present in registry 'iam-membership-events'
OK:  2 schemas present in registry 'iam-tenant-events'
OK: no orphan Glue schemas
OK: no orphan schemas/*.json
```

Failure modes and fixes:

- `missing Glue schemas ... DepartmentMembershipGranted ...`
  The registry does not yet have the schemas. Register them per registry:

  ```bash
  make schema-register REGISTRY=iam-membership-events
  make schema-register REGISTRY=iam-tenant-events
  make schema-verify
  ```

  If `schema-gov register` names schemas after the filename stem (already
  PascalCase in this repo — no override needed unlike the older User Profile
  runbook), the outputs should match without translation.

- The IAM role running `aws glue get-schema` returns AccessDenied.
  Ensure the caller can `glue:GetSchemaVersion` on **both** registry ARNs (see
  `deploy/iam/policy.json` sid `GlueSchemaRegistryReadOnly`).

- **Orphan detected in registry** (schema exists in Glue but no file in
  `schemas/`): use `schema-gov prune --mode archive` on the affected registry
  to move the schema into `docs/schema-archive/` and de-register from Glue.
  Never hard-delete without an archive — retention is required for
  event-payload replay from S3 archives.

## What happens if a schema is missing at pod startup

`NewGlueCodec` (per registry) returns an error of the form:

```
prefetch glue schema "DepartmentMembershipGranted" (event type
"department.membership.granted") in registry "iam-membership-events":
<underlying AWS error> — run `make schema-verify` to confirm the expected
schemas exist in both registries
```

The pod exits before opening the HTTP port. Kubernetes restart-loops it
(CrashLoopBackOff). Nothing publishes to SNS during this state; the outbox
runner is not started because pool wiring fails first in `cmd/server/main.go`.

**Mitigation:** roll back the Helm release with `helm rollback`, register the
missing schema, then re-deploy. Do NOT set the registry env vars to empty
strings to fall through to `NoopCodec` — that publishes unversioned JSON and
permanently corrupts the audit trail for the duration.

## Local development

`scripts/init-localstack.sh` registers all 14 schemas into LocalStack Glue mock
across the two registries under the same PascalCase names. LocalStack Pro is
required for Glue; without it the script logs and continues, and the app must
run with both `GLUE_REGISTRY_NAME_*` env vars empty (NoopCodec) — dev only,
never staging/prod.

## Adding a new event type

1. Add the payload struct and event-type constant to
   `internal/core/domain/events.go`.
2. Extend the `GlueSchemaName` switch with the new dot-notation → PascalCase
   mapping.
3. Decide the topic: `iam.membership.events` (default for org/dept/delegation)
   or `iam.tenant.events` (reserved; only added by explicit RFC — O&M is not
   the primary producer of tenant lifecycle beyond `TenantCreated` /
   `TrialStarted`).
4. Add the JSON schema file to
   `internal/adapter/outbound/eventbus/schemas/<PascalCase>.json` (matches file
   stem, no snake_case translation).
5. Update `api/asyncapi.yaml` — add the message under the correct channel.
6. Update `defaultSchemaEntries` in
   `internal/adapter/outbound/eventbus/validating_codec.go` (the ValidatingCodec
   registry table).
7. Update `RoutingPublisher` config in `cmd/server/main.go` if the new event
   requires a topic O&M does not already publish to (rare).
8. Update `scripts/init-localstack.sh` with the new `register_schema` call in
   the correct registry.
9. Run `make schema-validate` locally (or `make schema-verify` against a dev
   AWS account).
10. Merge, then run `make schema-register` in every environment before the
    first pod that would publish the new event starts.

The CI workflow `.github/workflows/schema-registry.yml` runs:

- On PRs: `extract --check` (detects `asyncapi.yaml` ↔ `schemas/` drift) +
  `validate` (all 8 passes) + `enforce-lifecycle` + `diff` against the target
  registries (both, per PR).
- On merges to `main`: `register` + `changelog --append` (writes
  `docs/schema-changelog.md`) + `metrics --push` (per registry).

Alerting for schema drift and registry health lives in
`deploy/monitoring/schema-registry-alerts.yml` (out-of-band):

- `schema_gov_orphan_registrations_total > 0` per registry (page).
- `schema_gov_missing_schemas_total > 0` per registry (page).
- Failed CI run on `main` (Slack warn only — no runtime impact).

## IAM policy — required SIDs

The pod's IAM role must include the following SIDs (see
`deploy/iam/policy.json`). Missing any of these produces `AccessDenied` at
startup.

**Glue Schema Registry (read, both registries):**

- `GlueSchemaRegistryReadOnly` — `glue:GetRegistry`, `glue:GetSchema`,
  `glue:GetSchemaVersion`, `glue:GetSchemaByDefinition`, `glue:ListSchemas`,
  `glue:QuerySchemaVersionMetadata` on both
  `${glue_registry_arn_membership}` + `/*` and
  `${glue_registry_arn_tenant}` + `/*`.

**SNS (publish, both topics):**

- `PublishMembershipEvents` — `sns:Publish` on `${sns_topic_arn_membership}`.
- `PublishTenantEvents` — `sns:Publish` on `${sns_topic_arn_tenant}`.

**SQS (consume, both queues + DLQs):**

- `ConsumeTenantLifecycleEvents` — `sqs:ReceiveMessage`, `sqs:DeleteMessage`,
  `sqs:GetQueueAttributes`, `sqs:ChangeMessageVisibility` on
  `${sqs_queue_arn_tenant_events}` (`tenant-orgm-q`) and its DLQ.
- `ConsumeBillingEvents` — same actions on
  `${sqs_queue_arn_billing_events}` (`billing-orgm-q`) and its DLQ.

**KMS:** `KMSForSNSAndSQSEncryption` — `kms:Encrypt` / `kms:Decrypt` /
`kms:GenerateDataKey` on the CMKs used by SNS and SQS. Exact action lists in
`policy.json`.

## Required env vars

Set in `deploy/helm/values.yaml` (per environment) or `.env-example` (dev):

| Variable | Purpose | Notes |
|---|---|---|
| `DATABASE_URL` | App pool DSN, RLS-enforced | Points at PgBouncer in prod/staging |
| `MIGRATION_DATABASE_URL` | Direct-Postgres DSN | Migrations require `pg_advisory_lock` (session-scoped) — must bypass PgBouncer |
| `GLUE_REGISTRY_NAME_MEMBERSHIP` | Membership-events registry | Set to `iam-membership-events` in production. Leave empty in dev (NoopCodec). |
| `GLUE_REGISTRY_NAME_TENANT` | Tenant-events registry | Set to `iam-tenant-events` in production. Leave empty in dev. |
| `SNS_TOPIC_ARN_MEMBERSHIP` | `iam.membership.events` topic ARN | Required. `RoutingPublisher` routing key `Envelope.Source == 'iam.membership.events'` |
| `SNS_TOPIC_ARN_TENANT` | `iam.tenant.events` topic ARN | Required. Only `TenantCreated` / `TrialStarted` published by O&M |
| `SQS_QUEUE_URL_TENANT_EVENTS` | `tenant-orgm-q` URL | RP tenant-lifecycle events (`TrialTenantProvisioned`, `TenantRealmReady`, ...) |
| `SQS_QUEUE_URL_BILLING_EVENTS` | `billing-orgm-q` URL | Billing events (`TenantPlanChanged`, `TenantSeatsChanged`, ...) |
| `AWS_REGION` | Primary region | `ap-south-1` in production |

## Runtime interaction with EVT-14 / EVT-15 / EVT-16

The schema-registry contract is enforced at publish time (ValidatingCodec) and
at consume time (schema-gov diff CI). It does **not** affect the runtime
recency / clock / relay guards, but a redriven DLQ event still needs a live
schema-version reference:

- **EVT-14** stale-skip does not de-register the schema; consumer still resolves
  the header schema version before comparing `event.time` vs
  `tenants.last_event_at`.
- **EVT-15** future-time reject NACKs to DLQ **without** ever validating the
  payload against the schema — clock-skewed events are refused before entering
  the projection.
- **EVT-16** `TenantStateChanged` relay writes go through the same
  ValidatingCodec path — the schema for `TenantStateChanged` in
  `iam-membership-events` must be present or the outbox insert fails and the
  consumer transaction rolls back (correct — no state without event).

If a runtime page implicates one of these guards, verify Glue reachability
first (`aws glue get-schema-version`) before treating it as a producer /
consumer logic bug.
