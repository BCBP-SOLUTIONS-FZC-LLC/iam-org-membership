#!/usr/bin/env bash
# Events/outbox/dedup CI gate: reject any application code that talks to
# SNS/SQS directly, or hand-builds an events.Envelope, instead of going
# through platform-events.
#
# The only legitimate way to publish is Publisher.Enqueue
# (internal/adapter/outbound/eventbus/publisher.go, which calls
# outbox.Enqueue(ctx, tx, env)); the only legitimate way to send to SNS is
# events.NewSNSPublisher (cmd/server/wiring.go's buildTopicPublisher); the
# only legitimate way to consume from SQS is events.NewSQSConsumerWithClient
# (cmd/server/wiring.go's buildSQSConsumer), and the only way to dead-letter
# is platform-events' DLQPublisher (events.NewSQSDLQPublisherWithClient +
# WithDLQForwarding).
# cmd/server/main.go is the one place allowed to construct a raw *sqs.Client
# — solely to hand it to those two constructors — and it must never call a
# transport method on it directly. There is deliberately no sns.NewFromConfig call
# anywhere in this repo — events.NewSNSPublisher builds its own SNS client
# internally.
#
# internal/adapter/outbound/eventbus/ is exempt from the transport-method
# check (#2) below: RoutingPublisher's own Publish/PublishBatch methods call
# through to the wrapped events.Publisher (platform-events' own interface,
# built via events.NewSNSPublisher) — same method names as the raw AWS SDK
# client, but the sanctioned call, not a bypass. Mirrors the identical
# exemption in check-outbox-access.sh for the same package.
#
# Consumer-side dedup (processed_events) goes through platform-events'
# pkg/inbox (inbox.Store.Process / Prune, wrapped by
# internal/adapter/outbound/postgres/idempotency_repository.go): check #4
# rejects hand-written SQL against that table anywhere else.
#
# Test files are exempt: e2e/integration tests legitimately talk to SNS/SQS
# directly to simulate external actors (Realm Provisioner, Billing, ...)
# that this service does not own.
set -euo pipefail

# ── 1. aws-sdk-go-v2 SNS/SQS packages may only be imported in:
#      • cmd/server/main.go    — constructs the raw *sqs.Client and hands it
#                                to events.NewSQSConsumerWithClient and
#                                events.NewSQSDLQPublisherWithClient.
#        (The former raw-SQS dlq_adapter.go is gone: platform-events v2's
#        DLQPublisher forwards to the RedrivePolicy DLQ itself.)
ALLOWED_SDK_IMPORT_FILES="cmd/server/main.go"

sdk_import_hits=$(grep -RIl -E '"github\.com/aws/aws-sdk-go-v2/service/(sns|sqs)(/[a-z0-9]+)*"' \
  --include='*.go' cmd/ internal/ pkg/ 2>/dev/null | grep -v '_test\.go$' || true)

for f in $sdk_import_hits; do
  allowed=false
  for a in $ALLOWED_SDK_IMPORT_FILES; do
    if [ "$f" = "$a" ]; then
      allowed=true
      break
    fi
  done
  if [ "$allowed" = false ]; then
    echo "::error file=${f}::Imports aws-sdk-go-v2 SNS/SQS directly. Only ${ALLOWED_SDK_IMPORT_FILES} may construct a raw client (to hand to platform-events) — publish via events.NewSNSPublisher, consume via events.NewSQSConsumerWithClient."
    exit 1
  fi
done

# ── 2. Direct SNS/SQS transport method calls are forbidden everywhere except:
#      • internal/adapter/outbound/eventbus/ — RoutingPublisher wraps
#        platform-events' own Publisher interface (same method names, not a
#        bypass — see header comment).
#   There is no other exception: immediate DLQ routing for EVT-15 poison
#   pills and schema violations goes through platform-events' DLQPublisher
#   (cmd/server/dlq.go). ─────────────────────────────────────────────────
method_hits=$(grep -REn '\.(SendMessage|SendMessageBatch|ReceiveMessage|DeleteMessage|DeleteMessageBatch|ChangeMessageVisibility|ChangeMessageVisibilityBatch|GetQueueAttributes|GetQueueUrl|Publish|PublishBatch|Subscribe)\(' \
  --include='*.go' cmd/ internal/ pkg/ 2>/dev/null \
  | grep -v '_test\.go' \
  | grep -v '^internal/adapter/outbound/eventbus/' \
  || true)

if [ -n "$method_hits" ]; then
  echo "::error::Direct SNS/SQS transport method call detected outside platform-events. Publish via eventbus.Publisher.Enqueue (outbox.Enqueue); consume via events.NewSQSConsumerWithClient's registered handler — never call the AWS SDK client's own methods."
  echo "$method_hits"
  exit 1
fi

# ── 3. events.Envelope must only ever be constructed via events.NewEnvelope,
#      never as a struct literal (which would skip its ID/time/schema
#      defaulting). ────────────────────────────────────────────────────────
#      Exempt: idempotency_repository.go builds an {ID, Type} envelope only
#      to hand the claim key to inbox.Store.Process — it is never published.
envelope_literal_hits=$(grep -REn 'events\.Envelope(\[[^]]*\])?\{' \
  --include='*.go' cmd/ internal/ pkg/ 2>/dev/null | grep -v '_test\.go' \
  | grep -v '^internal/adapter/outbound/postgres/idempotency_repository\.go:' || true)

if [ -n "$envelope_literal_hits" ]; then
  echo "::error::events.Envelope constructed as a struct literal instead of events.NewEnvelope(...). This skips platform-events' own ID/time/schema-version defaulting."
  echo "$envelope_literal_hits"
  exit 1
fi

# ── 4. processed_events is read and written only by platform-events' inbox
#      (claim, duplicate check, prune). Hand-written SQL against it would
#      reintroduce the probe-then-insert race inbox.Store.Process closes. ──
inbox_sql_hits=$(grep -REni '(from|into|update|join)[[:space:]]+(public\.)?processed_events' \
  --include='*.go' cmd/ internal/ pkg/ 2>/dev/null | grep -v '_test\.go' || true)

if [ -n "$inbox_sql_hits" ]; then
  echo "::error::Hand-written SQL against processed_events. Use port.Inbox (postgres.InboxRepository → platform-events inbox.Store) for dedup and port.InboxPruner for retention."
  echo "$inbox_sql_hits"
  exit 1
fi

echo "Events/outbox bypass check passed — no direct SNS/SQS transport calls or hand-built envelopes found outside platform-events."
