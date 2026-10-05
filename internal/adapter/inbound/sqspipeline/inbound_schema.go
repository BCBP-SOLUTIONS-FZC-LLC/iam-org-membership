package sqspipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// errSchemaViolation marks a consumed payload that fails its embedded JSON
// Schema. It is permanent — the same bytes fail on every redelivery — so
// routeRejectsToDLQ sends it straight to the DLQ.
var errSchemaViolation = errors.New("consumed event payload violates its embedded schema")

// Validator checks a consumed payload against this service's embedded
// schema for eventType. It returns nil for a valid payload AND for an event
// type with no embedded schema — the composition root adapts
// *eventbus.ValidatingCodec (whose ErrNoSchema means "not modelled") with
// ValidatorFunc, so this inbound package never imports an outbound one.
type Validator interface {
	Validate(eventType string, payload []byte) error
}

// ValidatorFunc adapts a function to Validator.
type ValidatorFunc func(eventType string, payload []byte) error

// Validate calls f.
func (f ValidatorFunc) Validate(eventType string, payload []byte) error { return f(eventType, payload) }

// validateConsumed checks each inbound envelope's (already Glue-decoded)
// payload against the embedded schema for env.Type before h runs. This is
// the consumer's own contract — what this service's asyncapi.yaml says it
// relies on — not the producer's Glue schema, so it needs no Glue call and
// keeps working through a Glue outage.
//
// An event type with no embedded schema passes through untouched (the
// Validator returns nil for it), so MembershipEventConsumer's ackUnknown
// forward-compat path still sees it.
// A violation is logged, counted as
// iam_org_membership_consumed_event_rejected_total{reason="schema_violation"},
// and returned as errSchemaViolation for the DLQ router.
func validateConsumed(h events.Handler, v Validator, log port.Logger) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		err := v.Validate(env.Type, env.Payload)
		if err == nil {
			return h(ctx, env)
		}
		metrics.IncConsumedEventRejected(env.Type, schemaViolationReason)
		if log != nil {
			log.Warn("consumed event violates its embedded schema — rejecting to DLQ",
				map[string]any{"event_id": env.ID, "event_type": env.Type, "source": env.Source, "error": err.Error()})
		}
		return fmt.Errorf("%w: %w", errSchemaViolation, err)
	}
}

// inboundHandler composes the full inbound pipeline for one queue,
// outermost first: DLQ routing (permanent rejects → DLQ + ack) →
// consumer span + queue context (instrumentedHandler) → consumed-schema
// validation → the consumer's own Handle. platform-events' consumer counts
// every delivery in platform_messages_*. A reject is counted once at its
// reject site (iam_org_membership_consumed_event_rejected_total{reason}),
// and its forward once by platform-events (platform_dlq_messages_total).
func inboundHandler(queue string, h events.Handler, v Validator, dlq events.DLQPublisher, log port.Logger) events.Handler {
	return routeRejectsToDLQ(instrumentedHandler(queue, validateConsumed(h, v, log)), dlq, log)
}

// Handler is the inbound pipeline for one queue (see inboundHandler).
func Handler(queue string, h events.Handler, v Validator, dlq events.DLQPublisher, log port.Logger) events.Handler {
	return inboundHandler(queue, h, v, dlq, log)
}
