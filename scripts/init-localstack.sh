#!/usr/bin/env bash
# LocalStack ready-hook — provisions the full O&M messaging topology per LLD
# §7.1, §7.3, §7.3.2 (rev 1.61). Runs automatically on container start via
# the volume mount to /etc/localstack/init/ready.d/.
#
# Creates:
#   3 SNS topics     — iam-membership-events, iam-tenant-events, billing-events
#   2 inbound SQS    — tenant-orgm-q, billing-orgm-q (+ DLQs, maxReceiveCount=5)
#   8 fan-out SQS    — §7.3.2 consumer queues on the two O&M topics (+ DLQs)
#   SNS→SQS subs     — with filter policies on the `EventType` attribute
#
# Community edition covers SNS + SQS; Glue Schema Registry is Pro-only.

set -euo pipefail

AWS_ACCOUNT=000000000000
AWS_REGION=ap-south-1
MAX_RECEIVES=5

# ────────────────────────────────────────────────────────────────────────
# Helpers
# ────────────────────────────────────────────────────────────────────────

topic_arn() { printf 'arn:aws:sns:%s:%s:%s' "$AWS_REGION" "$AWS_ACCOUNT" "$1"; }
queue_arn() { printf 'arn:aws:sqs:%s:%s:%s' "$AWS_REGION" "$AWS_ACCOUNT" "$1"; }
queue_url() { printf 'http://localhost:4566/%s/%s' "$AWS_ACCOUNT" "$1"; }

# create_queue_with_dlq <base-queue-name>
# Creates <base>-dlq then <base> with RedrivePolicy → <base>-dlq (max 5).
# Uses the JSON form of --attributes via a tempfile because awslocal's
# shorthand parser chokes on inline JSON values.
create_queue_with_dlq() {
  local queue="$1"
  local dlq="${queue}-dlq"

  awslocal sqs create-queue --queue-name "$dlq" >/dev/null
  local dlq_arn
  dlq_arn=$(queue_arn "$dlq")

  local attrs
  attrs=$(mktemp)
  cat > "$attrs" <<EOF
{
  "RedrivePolicy": "{\"deadLetterTargetArn\":\"${dlq_arn}\",\"maxReceiveCount\":\"${MAX_RECEIVES}\"}"
}
EOF
  awslocal sqs create-queue --queue-name "$queue" --attributes "file://$attrs" >/dev/null
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
  sub_arn=$(awslocal sns subscribe \
    --topic-arn "$t_arn" \
    --protocol sqs \
    --notification-endpoint "$q_arn" \
    --attributes RawMessageDelivery=true \
    --query 'SubscriptionArn' --output text)

  if [ -n "$filter" ]; then
    local attrs
    attrs=$(mktemp)
    printf '%s' "$filter" > "$attrs"
    awslocal sns set-subscription-attributes \
      --subscription-arn "$sub_arn" \
      --attribute-name FilterPolicy \
      --attribute-value "file://$attrs" >/dev/null
    rm -f "$attrs"
  fi
}

# ────────────────────────────────────────────────────────────────────────
# SNS topics (LLD §7.3)
# ────────────────────────────────────────────────────────────────────────

awslocal sns create-topic --name iam-membership-events >/dev/null
awslocal sns create-topic --name iam-tenant-events     >/dev/null
awslocal sns create-topic --name billing-events        >/dev/null

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
# Summary
# ────────────────────────────────────────────────────────────────────────

topic_count=$(awslocal sns list-topics --query 'length(Topics)' --output text)
queue_count=$(awslocal sqs list-queues --query 'length(QueueUrls)' --output text)
sub_count=$(awslocal sns list-subscriptions --query 'length(Subscriptions)' --output text)

echo "LocalStack init complete."
echo "  SNS topics:        ${topic_count} (expected 3)"
echo "  SQS queues+DLQs:   ${queue_count} (expected 20 = 10 pairs)"
echo "  SNS subscriptions: ${sub_count} (expected 10)"
