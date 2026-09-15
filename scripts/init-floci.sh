#!/usr/bin/env bash
# Floci ready-hook — provisions the full O&M messaging topology per LLD
# §7.1, §7.3, §7.3.2 (rev 1.61), plus both Glue Schema Registries (§7.3.1,
# SCHEMA-7). Runs automatically on container start via the volume mount to
# /etc/floci/init/ready.d/.
#
# Creates:
#   3 SNS topics     — iam-membership-events, iam-tenant-events, billing-events
#   2 inbound SQS    — tenant-orgm-q, billing-orgm-q (+ DLQs, maxReceiveCount=5)
#   8 fan-out SQS    — §7.3.2 consumer queues on the two O&M topics (+ DLQs)
#   SNS→SQS subs     — with filter policies on the `EventType` attribute
#   2 Glue registries — iam-membership-events, iam-tenant-events
#   14 Glue schemas  — every event this service produces (domain.IsProducedEvent),
#                      registered from the same JSON Schema Draft-07 files
#                      eventbus.GlueCodec ships, so GLUE_REGISTRY_MEMBERSHIP_NAME
#                      / GLUE_REGISTRY_TENANT_NAME can be set in .env and the
#                      app runs with the real Glue wire-format codec locally —
#                      Floci includes Glue Schema Registry in its free tier
#                      (unlike LocalStack Community, which gates it behind Pro),
#                      so there's no NoopCodec fallback needed for local dev.
#
# The docker-compose `floci` service mounts this repo's
# internal/adapter/outbound/eventbus/schemas/ directory read-only at
# /etc/floci/init/schemas so this script can read the schema definitions
# straight from source — one place to update when a schema changes.

set -euo pipefail

AWS_REGION=ap-south-1
AWS_ACCOUNT=000000000000
MAX_RECEIVES=5
SCHEMAS_DIR=/etc/floci/init/schemas

# The AWS CLI baked into the floci compat image defaults AWS_DEFAULT_REGION
# to us-east-1 regardless of the emulator's own FLOCI_DEFAULT_REGION — pin
# both region env vars so every `aws` call below lands in ap-south-1,
# matching FLOCI_DEFAULT_REGION/FLOCI_DEFAULT_ACCOUNT_ID on the container.
export AWS_DEFAULT_REGION="$AWS_REGION"
export AWS_REGION="$AWS_REGION"

# ────────────────────────────────────────────────────────────────────────
# Helpers
# ────────────────────────────────────────────────────────────────────────

topic_arn() { printf 'arn:aws:sns:%s:%s:%s' "$AWS_REGION" "$AWS_ACCOUNT" "$1"; }
queue_arn() { printf 'arn:aws:sqs:%s:%s:%s' "$AWS_REGION" "$AWS_ACCOUNT" "$1"; }
queue_url() { printf 'http://localhost:4566/%s/%s' "$AWS_ACCOUNT" "$1"; }

# create_queue_with_dlq <base-queue-name>
# Creates <base>-dlq then <base> with RedrivePolicy → <base>-dlq (max 5).
# Uses the JSON form of --attributes via a tempfile because the shorthand
# parser chokes on inline JSON values.
create_queue_with_dlq() {
  local queue="$1"
  local dlq="${queue}-dlq"

  aws sqs create-queue --queue-name "$dlq" >/dev/null
  local dlq_arn
  dlq_arn=$(queue_arn "$dlq")

  local attrs
  attrs=$(mktemp)
  cat > "$attrs" <<EOF
{
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"${dlq_arn}\",\"maxReceiveCount\":\"${MAX_RECEIVES}\"}"
}
EOF
  aws sqs create-queue --queue-name "$queue" --attributes "file://$attrs" >/dev/null
  rm -f "$attrs"
}

# subscribe_queue <topic-name> <queue-name> [filter-policy-json]
# Creates an SNS→SQS subscription and applies a filter policy on the
# `EventType` MessageAttribute if one is provided (§7.3.2).
subscribe_queue() {
  local topic="$1" queue="$2" filter="${3:-}"
  local t_arn q_arn
  t_arn=$(topic_arn "$topic")
  q_arn=$(queue_arn "$queue")

  local sub_arn
  sub_arn=$(aws sns subscribe \
    --topic-arn "$t_arn" \
    --protocol sqs \
    --notification-endpoint "$q_arn" \
    --attributes RawMessageDelivery=true \
    --query 'SubscriptionArn' --output text)

  if [ -n "$filter" ]; then
    local attrs
    attrs=$(mktemp)
    printf '%s' "$filter" > "$attrs"
    aws sns set-subscription-attributes \
      --subscription-arn "$sub_arn" \
      --attribute-name FilterPolicy \
      --attribute-value "file://$attrs" >/dev/null
    rm -f "$attrs"
  fi
}

# register_schema <registry> <schema-file> <schema-name>
# Idempotently creates the registry (ignores "already exists") and
# registers schema-file's contents as a new schema in it. DataFormat=JSON
# matches these files (JSON Schema Draft-07); Compatibility=BACKWARD
# mirrors the default schema-gov register uses against real AWS Glue.
register_schema() {
  local registry="$1" file="$2" name="$3"
  aws glue create-schema \
    --registry-id "RegistryName=${registry}" \
    --schema-name "$name" \
    --data-format JSON \
    --compatibility BACKWARD \
    --schema-definition "file://${SCHEMAS_DIR}/${file}" >/dev/null
}

# ────────────────────────────────────────────────────────────────────────
# SNS topics (LLD §7.3)
# ────────────────────────────────────────────────────────────────────────

aws sns create-topic --name iam-membership-events >/dev/null
aws sns create-topic --name iam-tenant-events     >/dev/null
aws sns create-topic --name billing-events        >/dev/null

# ────────────────────────────────────────────────────────────────────────
# Inbound SQS — this service reads these (LLD §7.1)
# ────────────────────────────────────────────────────────────────────────

create_queue_with_dlq tenant-orgm-q
create_queue_with_dlq billing-orgm-q

# tenant-orgm-q ← iam-tenant-events (no filter — consumes all RP-produced
# lifecycle events on this topic; produce/consume sets are disjoint so O&M
# never receives its own TenantCreated/TrialStarted, EVT-3).
subscribe_queue iam-tenant-events tenant-orgm-q

# billing-orgm-q ← billing-events (no filter — consumes all Billing events).
subscribe_queue billing-events billing-orgm-q

# ────────────────────────────────────────────────────────────────────────
# Downstream fan-out — LLD §7.3.2, one queue per subscribing service.
# In real deployments these belong to the consumer services; provisioning
# them here lets local dev / integration tests observe the full fan-out.
# ────────────────────────────────────────────────────────────────────────

# iam-membership-events fan-out ------------------------------------------

# Audit Log — catch-all (no filter).
create_queue_with_dlq membership-audit-q
subscribe_queue iam-membership-events membership-audit-q

# AuthZ Enrichment — membership & tenant-role events only.
create_queue_with_dlq membership-authz-q
subscribe_queue iam-membership-events membership-authz-q \
  '{"EventType": ["DepartmentMembershipGranted","DepartmentMembershipRevoked","DepartmentMembershipLevelChanged","TenantRoleGranted","TenantRoleRevoked"]}'

# Realm Provisioner — dept-approver + tenant-owner/admin transitions
# (drives the requires-mfa realm role, §6.5).
create_queue_with_dlq membership-realm-q
subscribe_queue iam-membership-events membership-realm-q \
  '{"EventType": ["DepartmentMembershipGranted","DepartmentMembershipLevelChanged","TenantRoleGranted","TenantRoleRevoked"]}'

# Notification — everything that generates user/admin emails or banners.
create_queue_with_dlq membership-notification-q
subscribe_queue iam-membership-events membership-notification-q \
  '{"EventType": ["DepartmentMembershipGranted","DepartmentMembershipRevoked","DepartmentMembershipLevelChanged","TenantRoleGranted","TenantRoleRevoked","DelegationStarted","DelegationEnded","TenantSeatOverageStarted","TenantSeatOverageResolved"]}'

# Workflow Service — delegation + override + membership + tenant-state relay.
create_queue_with_dlq membership-workflow-q
subscribe_queue iam-membership-events membership-workflow-q \
  '{"EventType": ["DelegationStarted","DelegationEnded","TenderAssigneeOverridden","DepartmentMembershipGranted","DepartmentMembershipRevoked","DepartmentMembershipLevelChanged","TenantStateChanged"]}'

# Billing Service — narrow filter, seat-overage only (§16 A59, SEAT-3/5).
create_queue_with_dlq membership-billing-q
subscribe_queue iam-membership-events membership-billing-q \
  '{"EventType": ["TenantSeatOverageStarted","TenantSeatOverageResolved"]}'

# iam-tenant-events fan-out ---------------------------------------------

# Audit Log — includes all tenant-lifecycle events (other producers too).
create_queue_with_dlq tenant-audit-q
subscribe_queue iam-tenant-events tenant-audit-q

# Notification — welcome / trial-start emails.
create_queue_with_dlq tenant-notification-q
subscribe_queue iam-tenant-events tenant-notification-q \
  '{"EventType": ["TenantCreated","TrialStarted"]}'

# ────────────────────────────────────────────────────────────────────────
# Glue Schema Registry (LLD §7.3.1, SCHEMA-7) — one registry per SNS topic,
# populated from the exact JSON Schema Draft-07 files eventbus.GlueCodec
# reads in production, so local dev exercises the real Glue wire format
# instead of NoopCodec.
# ────────────────────────────────────────────────────────────────────────

aws glue create-registry --registry-name iam-membership-events >/dev/null
aws glue create-registry --registry-name iam-tenant-events     >/dev/null

register_schema iam-membership-events department_membership_granted.json       DepartmentMembershipGranted
register_schema iam-membership-events department_membership_level_changed.json DepartmentMembershipLevelChanged
register_schema iam-membership-events department_membership_revoked.json       DepartmentMembershipRevoked
register_schema iam-membership-events membership_revoked.json                  MembershipRevoked
register_schema iam-membership-events mfareset.json                            MFAReset
register_schema iam-membership-events tenant_memberships_purged.json           TenantMembershipsPurged
register_schema iam-membership-events tenant_role_granted.json                 TenantRoleGranted
register_schema iam-membership-events tenant_role_revoked.json                 TenantRoleRevoked
register_schema iam-membership-events tenant_seat_overage_resolved.json        TenantSeatOverageResolved
register_schema iam-membership-events tenant_seat_overage_started.json         TenantSeatOverageStarted
register_schema iam-membership-events tenant_state_changed.json                TenantStateChanged
register_schema iam-membership-events tender_assignee_overridden.json          TenderAssigneeOverridden

register_schema iam-tenant-events tenant_created.json TenantCreated
register_schema iam-tenant-events trial_started.json  TrialStarted

# ────────────────────────────────────────────────────────────────────────
# Summary
# ────────────────────────────────────────────────────────────────────────

topic_count=$(aws sns list-topics --query 'length(Topics)' --output text)
queue_count=$(aws sqs list-queues --query 'length(QueueUrls)' --output text)
sub_count=$(aws sns list-subscriptions --query 'length(Subscriptions)' --output text)
schema_count=$(( \
  $(aws glue list-schemas --registry-id RegistryName=iam-membership-events --query 'length(Schemas)' --output text) + \
  $(aws glue list-schemas --registry-id RegistryName=iam-tenant-events --query 'length(Schemas)' --output text) \
))

echo "Floci init complete."
echo "  SNS topics:        ${topic_count} (expected 3)"
echo "  SQS queues+DLQs:   ${queue_count} (expected 20 = 10 pairs)"
echo "  SNS subscriptions: ${sub_count} (expected 10)"
echo "  Glue schemas:      ${schema_count} (expected 14 across 2 registries)"
