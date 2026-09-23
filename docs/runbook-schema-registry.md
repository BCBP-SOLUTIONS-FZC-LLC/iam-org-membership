# Runbook — AWS Glue Schema Registry state (two registries)

## The contract

`iam-org-membership` publishes to two SNS topics, each backed by its own AWS
Glue Schema Registry (SCHEMA-7, LLD §7.3). At startup,
`eventbus.NewGlueCodec` (one per registry, `internal/adapter/outbound/eventbus/glue_codec.go`)
resolves each produced schema's version UUID **by definition**:
`glue:GetSchemaByDefinition` with this binary's own embedded
`schemas/*.json` file, sent in exactly the compact form `schema-gov register`
uploads (`registeredDefinition`). The version must be `AVAILABLE`.

- Each build therefore stamps the version that actually describes its
  payloads — never merely the registry's latest, which runs ahead of the
  running code when a schema is registered before deploy (or after a
  rollback) and behind it when a deploy races `schema-registry.yml`.
- The UUID is fixed for the life of the pod. There is **no refresher**, and
  publishing makes no Glue call.
- Startup fails fast if any produced schema's definition isn't registered —
  the pod will not accept traffic. Stamping events with a version that doesn't
  describe them is worse than not starting.

On the consume side no Glue call is made at all: every inbound queue carries
`eventbus.GlueDecoder` (`events.WithConsumerCodec`), which strips the
self-describing 18-byte header (`[0x03][0x00][16-byte version UUID]`) without
a registry. Consumed payloads are then validated against this service's own
**embedded** consumed schemas (`cmd/server/inbound_schema.go`), not the
producer's Glue schema — see "Consumed-schema violations" below.

### Schema files and names

`schema-gov extract` writes snake_case files into
`internal/adapter/outbound/eventbus/schemas/` (e.g. `tenant_role_granted.json`)
— 28 files: the 14 this service **produces** plus 14 **consumed**,
other-service-owned extracts kept for schema-gov coverage and consumer-side
validation. The registered Glue schema name (= `envelope.type`) is PascalCase.
The filename → name mapping is hand-maintained in two places, one line per
event:

- `schemaFileNames` in `glue_codec.go` (runtime: embedding, validation, Glue
  lookup), and
- `.github/scripts/stage-produced-event-schemas.sh` (CI/`make schema-register`:
  stages only the produced subset, renamed to PascalCase, per registry lane).

Consumed schemas are never registered from this repo.

### Registry: `iam-membership-events` (env `GLUE_REGISTRY_MEMBERSHIP_NAME`)

Backs SNS topic `iam.membership.events`. Twelve schemas:

`DepartmentMembershipGranted`, `DepartmentMembershipRevoked`,
`DepartmentMembershipLevelChanged`, `TenantRoleGranted`, `TenantRoleRevoked`,
`MembershipRevoked`, `TenantMembershipsPurged`, `TenderAssigneeOverridden`
(I-13), `MFAReset` (P-34), `TenantSeatOverageStarted`,
`TenantSeatOverageResolved`, `TenantStateChanged` (EVT-16 relay).

### Registry: `iam-tenant-events` (env `GLUE_REGISTRY_TENANT_NAME`)

Backs SNS topic `iam.tenant.events` (shared with Realm Provisioner — disjoint
schema names). Two schemas — the only tenant-lifecycle events O&M
**produces**: `TenantCreated`, `TrialStarted`. Everything else on that topic is
Realm-Provisioner-produced and consumed here via `tenant-orgm-q`.

Total: **14 produced schemas across two registries.** Topic routing is keyed
on event type via `domain.TopicForEvent`, not `Envelope.Source`.

## Pre-deploy checks

```bash
export GLUE_REGISTRY_MEMBERSHIP_NAME=iam-membership-events   # or the env-specific name
export GLUE_REGISTRY_TENANT_NAME=iam-tenant-events
export AWS_REGION=ap-south-1
make schema-validate   # schema-gov extract + validate (8 passes), no AWS needed
make schema-verify     # get-schema-by-definition for all 14 schemas across both registries
```

`make schema-validate` runs `platform-schemagov:0.4` against
`api/asyncapi.yaml` + `schemas/*.json`: JSON structure, Draft-07 meta-schema,
enum semantic drift, lifecycle annotations, open-schema guard, consumer
strict-mode scan, AsyncAPI↔schema coverage, AsyncAPI structure. It also
deletes the stray `mfareset.json` `extract` always writes (known schema-gov
0.4 naming inconsistency; this repo keeps `mfa_reset.json`).

`make schema-verify` runs the exact lookup a pod runs at startup: for every
produced schema it calls `aws glue get-schema-by-definition` with this
checkout's file (serialized the way `schema-gov register` uploads it) and
fails unless the matched version is `AVAILABLE` — so it also catches a schema
registered from a *different* commit, which a name-only check could not
(`OK: all 14 schema definitions registered and AVAILABLE across both
registries`). Needs `aws` and `python3`.

Failure modes and fixes:

- `FAIL: this checkout's schema definition is not registered+AVAILABLE: ...`
  — each entry is `registry:Name(status)`; `not-registered` means no version
  has this definition. Register them: `make schema-register`
  (stages the produced schemas per lane and registers both registries), then
  re-run `make schema-verify`.
- `aws glue get-schema-by-definition` returns AccessDenied — the caller needs the
  `GlueSchemaRegistryReadOnly` actions below on **both** registry ARNs.

## What happens if a schema is missing at pod startup

`NewGlueCodec` (per registry) returns an error of the form:

```
resolve glue schema "DepartmentMembershipGranted" in registry
"iam-membership-events" by definition: <underlying AWS error> — this
binary's schema version isn't registered yet: wait for schema-registry.yml
to register it, or run `make schema-verify`
```

`cmd/server/main.go` panics before opening the HTTP port and Kubernetes
restart-loops the pod (CrashLoopBackOff). Nothing publishes to SNS in this
state; the outbox keeps accumulating rows.

Common causes:

- **The deploy outran `schema-registry.yml`** for a release that changed a
  schema. The pod recovers on its own on the first restart after
  registration lands — no action needed beyond letting the workflow finish.
- **The embedded definition differs from every registered version** — e.g. a
  `schemas/*.json` file edited without `make extract-schemas`, or a
  registration that failed Glue's BACKWARD check. Fix the schema and let
  `schema-registry.yml` register it.
- The version exists but is `PENDING`/`FAILURE` in Glue (compatibility check
  still running or failed).

Do NOT set the registry env vars to empty strings to fall through to
`NoopCodec` in a real environment — that publishes unversioned JSON.

## Consumed-schema violations and the DLQ

Each inbound queue runs, outermost first: DLQ router (`cmd/server/dlq.go`) →
`platform_messages_*` counters → consumed-schema validation → `Handle`.
Permanent rejects are `SendMessage`'d straight to the queue's DLQ (DLQ URL
read from the queue's own `RedrivePolicy`) with message attributes
`EventType` and `DLQReason`, then acked:

| `DLQReason` | Cause | Alert |
|---|---|---|
| `future_time_clamp` | EVT-15: `event.time > now() + MAX_LIFECYCLE_EVENT_SKEW_SECONDS` | `IAMFutureLifecycleEventRejected` |
| `schema_violation` | Decoded payload fails its embedded consumed schema (EVT-17) | `IAMConsumedEventSchemaViolation` |

Both increment `platform_dlq_messages_total{event_type,reason}`. If the DLQ
can't be resolved at startup (logged as `DLQ routing disabled`) or a send
fails, the message falls back to normal SQS retry + redrive after
`maxReceiveCount=5`. `catalog-orgm-q` is inert today and has no
`deploy/iam/policy.json` grant, so it always takes the fallback.

On a `schema_violation` page: inspect a DLQ message, decide which side is
wrong — the producer broke its contract, or this repo's `api/asyncapi.yaml`
is stale/stricter than what the producer sends — fix it (for the latter:
edit `api/asyncapi.yaml`, `make extract-schemas`, `make schema-validate`),
then redrive. A redriven message is decoded as plain JSON (the router clears
`dataschema`) and re-validated.

## Local development

`scripts/init-floci.sh` registers all 14 produced schemas into floci's Glue
Schema Registry across the two registries, reading the definitions straight
from `internal/adapter/outbound/eventbus/schemas/` (mounted read-only into
the container). floci matches `GetSchemaByDefinition` by JSON content, so the
pretty-printed files it registers resolve the same as CI's compact uploads.
`GLUE_REGISTRY_*_NAME` is set by default in `.env-example`, so `make
docker-up` runs the real Glue wire-format codec end to end.
`test/integration/glue_codec_test.go` exercises the by-definition lookup
against floci (a build keeps stamping its own version after a newer one is
registered; an unregistered definition fails startup).

## Adding a new produced event type

1. Add the event-type constant and payload to `internal/core/domain`, and to
   `domain.IsProducedEvent` / `domain.TopicForEvent` (topic choice:
   `iam.membership.events` by default; `iam.tenant.events` only by explicit
   decision).
2. Add the message under the correct channel in `api/asyncapi.yaml`, then
   `make extract-schemas` (never hand-edit the extracted JSON).
3. Add the new file to `schemaFileNames` (`glue_codec.go`) and to
   `.github/scripts/stage-produced-event-schemas.sh` (`make schema-verify`
   reads the staged files, so it needs no list of its own); add the name to
   `scripts/init-floci.sh`.
4. Run `make schema-validate` and the unit tests (the Python parity test
   checks the new file's registered form).
5. Merge. `schema-registry.yml` registers it; pods built from that commit
   start once registration lands.

The CI workflow `.github/workflows/schema-registry.yml` runs, per registry
(matrix): on PRs — validate + diff against the staging registry (read-only);
on push to `main` / release — validate, freeze check, orphan detection,
usage→lifecycle enforcement, diff, `register`, changelog, metrics.

Alerting for registry health lives in
`deploy/monitoring/schema-registry-alerts.yml` (`SchemaBreakingChangeBlocked`,
`SchemaOrphanCountHigh`, `SchemaRegistrationSpike`, `SchemaVersionChurn`, …).

## IAM policy — required statements

The pod's IAM role (`deploy/iam/policy.json`):

- **`GlueSchemaRegistryReadOnly`** — `glue:GetRegistry`, `glue:GetSchema`,
  `glue:GetSchemaVersion`, `glue:GetSchemaByDefinition` (the startup lookup),
  `glue:ListSchemas`, `glue:QuerySchemaVersionMetadata` on both registry ARNs
  and their schemas.
- **`PublishMembershipEvents`** — `sns:Publish`, `sns:GetTopicAttributes` on
  both topics.
- **`ConsumeTenantAndBillingQueues`** — `sqs:ReceiveMessage`,
  `sqs:DeleteMessage`, `sqs:GetQueueAttributes` (also used to read each
  queue's `RedrivePolicy`), `sqs:ChangeMessageVisibility`, `sqs:SendMessage`
  (straight-to-DLQ rejects) on `tenant-orgm-q`, `billing-orgm-q` and their
  DLQs.

## Required env vars

| Variable | Purpose | Notes |
|---|---|---|
| `GLUE_REGISTRY_MEMBERSHIP_NAME` / `_ARN` | `iam-membership-events` registry | Unset ⇒ `NoopCodec` (plain JSON) on that topic |
| `GLUE_REGISTRY_TENANT_NAME` / `_ARN` | `iam-tenant-events` registry | Unset ⇒ `NoopCodec` on that topic |
| `SNS_TOPIC_MEMBERSHIP_ARN` | `iam.membership.events` | Unset ⇒ noop publisher (dev) |
| `SNS_TOPIC_TENANT_ARN` | `iam.tenant.events` | Only `TenantCreated` / `TrialStarted` |
| `SQS_TENANT_ORGM_QUEUE_URL` | `tenant-orgm-q` | RP tenant-lifecycle events |
| `SQS_BILLING_ORGM_QUEUE_URL` | `billing-orgm-q` | Billing events |
| `SQS_CATALOG_ORGM_QUEUE_URL` | `catalog-orgm-q` | Unset everywhere today (inert) |
| `AWS_REGION` | Region | `ap-south-1` |

No env var configures DLQs — they come from each queue's `RedrivePolicy`.

## Runtime interaction with EVT-14 / EVT-15 / EVT-16

- **EVT-14** stale-skip runs inside `Handle`, after decode and validation; a
  stale event still passes schema validation and is recorded in
  `processed_events`.
- **EVT-15** future-time reject happens inside `Handle` too; the payload has
  already passed consumed-schema validation, and the DLQ router moves the
  message straight to the DLQ (`DLQReason=future_time_clamp`) without
  recording `processed_events`.
- **EVT-16** `TenantStateChanged` relay writes go through the same
  `ValidatingCodec` enqueue path — a relay payload that fails its produced
  schema fails the outbox insert and rolls back the consumer transaction
  (correct — no state without event).
