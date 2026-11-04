// Package sqspipeline is the inbound SQS pipeline every queue of this
// service runs: consumer construction with its defaults (DLQ forwarding,
// receive count, drain/handler timeouts, queue-depth sampling, Glue decode),
// the permanent-reject DLQ shortcut, consumed-schema validation and the
// consumer span. It lives outside cmd/server so integration tests can run
// the production pipeline against a real SQS (floci).
package sqspipeline

import (
	"context"
	"encoding/json"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// dlqForwardReceiveCount is the consumer-side delivery threshold applied
// when SQS_MAX_RECEIVE_COUNT is unset. With DLQ forwarding on, platform-events
// forwards a message to the DLQ once it reaches this count, and it must be
// strictly below the queue's RedrivePolicy maxReceiveCount (5 for every
// inbound queue here, scripts/init-floci.sh / platform Terraform), or SQS
// moves the message first and the library's forward never happens.
const dlqForwardReceiveCount = 4

// Consumer defaults applied when the env leaves them unset.
const (
	// sqsDrainTimeout bounds each consumer's Stop; the shutdown sequence in
	// main stops all consumers in parallel with the HTTP drain, inside
	// terminationGracePeriodSeconds (see main's shutdown comment).
	sqsDrainTimeout = 15 * time.Second
	// sqsHandlerTimeout bounds one handler call (e.g. the Catalog lookup on
	// TrialReactivated), below the 30s SQS visibility timeout so a hung
	// handler fails and is retried instead of hiding its message forever.
	sqsHandlerTimeout = 20 * time.Second
	// sqsQueueDepthInterval enables platform-events' platform_queue_depth /
	// platform_dlq_depth sampler (one GetQueueAttributes per queue and DLQ
	// per replica per interval). IAMOrgMembershipDLQBacklog alerts on it.
	sqsQueueDepthInterval = 60 * time.Second
)

// sqsEnvForQueue copies the shared LoadSQS() values (region, endpoint,
// long-poll, visibility, max receive count) onto one inbound queue, then
// overlays that queue's URL and concurrency. LoadSQS only reads SQS_QUEUE_URL
// / SQS_CONCURRENCY; this service has three queues with distinct env names.
// An unset max receive count becomes dlqForwardReceiveCount; unset drain and
// handler timeouts become sqsDrainTimeout / sqsHandlerTimeout, and the
// queue-depth sampler runs every sqsQueueDepthInterval.
func sqsEnvForQueue(base eventcfg.SQSConfigEnv, queueURL string, concurrency int) eventcfg.SQSConfigEnv {
	env := base
	env.QueueURL = queueURL
	if concurrency > 0 {
		env.Concurrency = concurrency
	}
	if env.MaxReceiveCount <= 0 {
		env.MaxReceiveCount = dlqForwardReceiveCount
	}
	if env.DrainTimeout <= 0 {
		env.DrainTimeout = sqsDrainTimeout
	}
	if env.HandlerTimeout <= 0 {
		env.HandlerTimeout = sqsHandlerTimeout
	}
	if env.QueueDepthInterval <= 0 {
		env.QueueDepthInterval = sqsQueueDepthInterval
	}
	return env
}

// buildSQSConsumer constructs a platform-events SQS consumer from a
// LoadSQS-derived env (after sqsEnvForQueue) plus SQSConsumerOptions
// (concurrency, visibility timeout, max-receive-count) and platform-events'
// decode-only events.GlueDecodeCodec. Upstream producers (Realm Provisioner,
// Billing, Catalog) Glue-frame their payloads in their own registries; the
// codec strips the self-describing 18-byte header (no registry client) and
// rejects compressed or non-JSON payloads as a decode_error, which the
// library forwards to the DLQ. Plain-JSON envelopes (no dataschema) never
// reach it.
//
// With dlq non-nil, platform-events forwards poison messages to the queue's
// RedrivePolicy DLQ itself, preserving the original body and attributes and
// deleting only after the forward succeeds: a body that is not a valid
// envelope, a payload whose Glue decode fails, and a message past the
// max-receive threshold. Without it, a malformed body is deleted and only
// logged, which on tenant-orgm-q would silently drop a lifecycle event
// (offboarding included). Start then also fails on a queue with no usable
// RedrivePolicy. dlq is nil only in tests that exercise decoding alone.
func buildSQSConsumer(env eventcfg.SQSConfigEnv, client events.SQSClientLike, handler events.Handler, dlq events.DLQPublisher, log port.Logger) (events.Consumer, error) {
	opts := append(eventcfg.SQSConsumerOptions(env), events.WithConsumerCodec(events.GlueDecodeCodec{}))
	if dlq != nil {
		opts = append(opts, events.WithDLQForwarding(dlq))
	}
	return events.NewSQSConsumerWithClient(eventcfg.SQSConfigFromEnv(env, log), client, handler, opts...)
}

// instrumentedHandler wraps an SQS handler in an internal span named
// "consumer.<event_type>" (OBS-7/OBS-9), a child of platform-events'
// sqs.receive consumer span. The platform_messages_* counters and the
// sqs.receive span (linked to the SNS publish span through the traceparent
// message attribute) are recorded by platform-events itself.
//
// The publish span belongs to the outbox runner, not to the request that
// wrote the event, so this span adds a link to that originating request:
// publisher.go stamps its trace ID (trace_id) and span ID (correlation_id)
// on the envelope. A link needs both; an envelope carrying only a trace ID
// gets no link. The sampled flag is left unset: the link points at the
// producer's span whatever its sampling decision was.
//
// The concrete span creation is delegated to port.WrapConsumerSpan, which
// calls the closure installed by cmd/server (port.SetConsumerSpanFunc) after
// gincommon.InitTracingWithConfig — keeping the OTel trace API out of this
// package. When no function is installed (tests, reconciler) the handler
// runs without a span.
//
// queue is the span's messaging.destination attribute.
func instrumentedHandler(queue string, h events.Handler) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		link := port.SpanContext{TraceID: env.TraceID, SpanID: env.CorrelationID}
		ctx, endSpan := port.WrapConsumerSpan(ctx, "consumer."+env.Type, queue, link)
		err := h(ctx, env)
		endSpan(err)
		return err
	}
}

// EnvForQueue is sqsEnvForQueue: one queue's consumer env with this
// service's defaults.
func EnvForQueue(base eventcfg.SQSConfigEnv, queueURL string, concurrency int) eventcfg.SQSConfigEnv {
	return sqsEnvForQueue(base, queueURL, concurrency)
}

// NewConsumer is buildSQSConsumer: a platform-events SQS consumer with this
// service's codec and DLQ forwarding.
func NewConsumer(env eventcfg.SQSConfigEnv, client events.SQSClientLike, handler events.Handler, dlq events.DLQPublisher, log port.Logger) (events.Consumer, error) {
	return buildSQSConsumer(env, client, handler, dlq, log)
}
