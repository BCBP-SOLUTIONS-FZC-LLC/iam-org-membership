# AWS IAM policy for iam-org-membership

This directory holds the reference IAM policy that must be attached to the
service's IRSA role. The application does not create the role itself — that is
managed by platform Terraform. Copy the JSON in `policy.json` into the
Terraform module or apply it directly via `aws iam create-policy` /
`aws iam put-role-policy`.

The role is assumed by the service's Kubernetes ServiceAccount via IRSA. Wire
the ARN into Helm via `serviceAccount.annotations`:

```yaml
serviceAccount:
  create: true
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::ACCOUNT_ID:role/iam-org-membership
```

The SAME role is assumed by every CronJob rendered from
`deploy/helm/templates/cronjobs.yaml` (all 8 reconcilers share the pod
ServiceAccount). None of them need permissions beyond the base set.

## What org-membership needs (and does NOT need)

Org-membership is a database-of-record service that publishes events and
consumes lifecycle projections. It does **not** store binary blobs, does
**not** encrypt anything with a service-owned KMS key (RDS handles at-rest
encryption transparently), and does **not** touch S3. If you find yourself
adding `s3:*` or `kms:*` to this policy, stop — you are almost certainly in
the wrong repo (User Profile owns signature storage + KMS).

## Grants breakdown

| Sid | AWS service | Actions | Purpose | LLD ref |
|---|---|---|---|---|
| `PublishMembershipEvents` | SNS | `Publish`, `GetTopicAttributes` | RoutingPublisher fans out to `iam-membership-events` AND `iam-tenant-events` based on `Envelope.Source`. Both topic ARNs are required. | §7.3 |
| `ConsumeTenantAndBillingQueues` | SQS | `ReceiveMessage`, `DeleteMessage`, `GetQueueAttributes`, `ChangeMessageVisibility`, `SendMessage` | Two SQS consumers (`tenant-orgm-q`, `billing-orgm-q`) subscribe to upstream tenant/billing topics. `SendMessage` is needed on the DLQs for the EVT-15 future-time reject path and for manual DLQ redrive. | §7.1 |
| `GlueSchemaRegistryReadOnly` | Glue | `GetRegistry`, `GetSchema`, `GetSchemaVersion`, `GetSchemaByDefinition`, `ListSchemas`, `QuerySchemaVersionMetadata` | The Glue codec pre-fetches schema version IDs on startup and validates every published envelope. Two registries: `iam-membership-events` and `iam-tenant-events`. Schema-registration is CI-only and lives on a separate role. | §7.3.1 |
| `CloudWatchMetricsForExporterGoroutines` | CloudWatch | `PutMetricData` (scoped to namespace `BCBP/OrgMembership`) | The four exporter goroutines started alongside the outbox runner (`iam_tenant_ownerless`, `iam_realm_sync_pending`, `iam_seat_overage_active`, `iam_pending_invitations_stale`) emit gauges. Prometheus scrapes them via ServiceMonitor; CloudWatch mirroring is optional and namespace-scoped for cardinality safety. | §11.2 |
| `CloudWatchLogs` | Logs | `CreateLogStream`, `PutLogEvents` | Container stdout when the OTel collector is not in the log pipeline. | — |

## Explicitly out of scope

- **No S3.** Org-membership never reads, writes, or lists S3 objects. If a
  signature URL appears in an API response, it originated from User Profile.
- **No KMS.** RDS-at-rest encryption uses an AWS-managed key; the pod does
  not participate. No client-side envelope encryption is performed.
- **No STS AssumeRole beyond IRSA.** The pod does not chain-assume any
  cross-account roles.
- **No Keycloak Admin API access.** All Keycloak mutations go through the
  Realm Provisioner service; org-membership calls RP over HTTPS with the
  ambient service mesh mTLS.

## Rolling out

Order of operations for production:

1. Apply the new policy in Terraform / IAM console.
2. Verify each grant from within a debug pod that shares the ServiceAccount:
   ```bash
   aws sns publish --topic-arn <membership_events_topic_arn> --message test  # expect 200
   aws sqs get-queue-attributes --queue-url <tenant_orgm_queue_url>            # expect Attributes
   aws glue get-registry --registry-id.RegistryName=iam-membership-events     # expect a Registry object
   aws cloudwatch put-metric-data --namespace BCBP/OrgMembership \
       --metric-name TestMetric --value 1                                      # expect no output
   ```
3. Deploy the new image + Helm chart.
4. Watch `outbox_dead_letters_total` — a rising rate immediately after rollout
   means the policy did not apply (SNS Publish denied). Roll back if so.

## Least-privilege scoping

The SNS topic policies should also be tightened alongside the IAM policy —
each topic's resource policy should permit only this role and Workflow's
subscription filter. That is managed by platform infrastructure and is not
represented here.
