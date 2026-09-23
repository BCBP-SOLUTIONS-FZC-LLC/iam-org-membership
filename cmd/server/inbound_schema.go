package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// errSchemaViolation marks a consumed payload that fails its embedded JSON
// Schema. It is permanent — the same bytes fail on every redelivery — so
// routeRejectsToDLQ sends it straight to the DLQ.
var errSchemaViolation = errors.New("consumed event payload violates its embedded schema")

// payloadValidator is satisfied by *eventbus.ValidatingCodec — the same
// compiled schemas (embedded schemas/*.json) that gate produced events at
// outbox-enqueue time.
type payloadValidator interface {
	Validate(eventType string, payload []byte) error
}

// validateConsumed checks each inbound envelope's (already Glue-decoded)
// payload against the embedded schema for env.Type before h runs. This is
// the consumer's own contract — what this service's asyncapi.yaml says it
// relies on — not the producer's Glue schema, so it needs no Glue call and
// keeps working through a Glue outage.
//
// An event type with no embedded schema passes through untouched, so
// MembershipEventConsumer's ackUnknown forward-compat path still sees it.
// A violation is logged, counted as
// platform_dlq_messages_total{reason="schema_violation"}, and returned as
// errSchemaViolation for the DLQ router.
func validateConsumed(h events.Handler, v payloadValidator, log port.Logger) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		err := v.Validate(env.Type, env.Payload)
		if err == nil || errors.Is(err, eventbusadapter.ErrNoSchema) {
			return h(ctx, env)
		}
		if metrics.DLQMessages != nil {
			metrics.DLQMessages.WithLabelValues(env.Type, schemaViolationReason).Inc()
		}
		if log != nil {
			log.Warn("consumed event violates its embedded schema — rejecting to DLQ",
				map[string]any{"event_id": env.ID, "event_type": env.Type, "source": env.Source, "error": err.Error()})
		}
		return fmt.Errorf("%w: %w", errSchemaViolation, err)
	}
}

// inboundHandler composes the full inbound pipeline for one queue,
// outermost first: DLQ routing (permanent rejects → DLQ + ack) →
// platform_messages_* counters (so rejects still count as failed) →
// consumed-schema validation → the consumer's own Handle.
func inboundHandler(ctx context.Context, client dlqSQSClient, queueURL, queue string, h events.Handler, v payloadValidator, log port.Logger) events.Handler {
	return withDLQRouting(ctx, client, queueURL, instrumentedHandler(queue, validateConsumed(h, v, log)), log)
}
