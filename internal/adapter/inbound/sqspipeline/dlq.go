package sqspipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	consumeradapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// DLQReason values for the permanent rejects this service routes itself.
// They travel as the forwarded message's DLQReason attribute and as the
// reason label of iam_org_membership_consumed_event_rejected_total, counted
// at each reject site.
const (
	poisonPillReason           = "future_time_clamp"
	schemaViolationReason      = "schema_violation"
	provisioningRejectedReason = "provisioning_rejected"
)

// dlqStartupTimeout bounds the startup DLQ preflight per queue.
const dlqStartupTimeout = 5 * time.Second

// dlqReason reports whether err is a permanent reject that belongs in the
// DLQ now rather than after maxReceiveCount retries, and under which reason.
func dlqReason(err error) (string, bool) {
	switch {
	case errors.Is(err, consumeradapter.ErrPoisonPill):
		return poisonPillReason, true
	case errors.Is(err, errSchemaViolation):
		return schemaViolationReason, true
	case errors.Is(err, consumeradapter.ErrProvisioningRejected):
		return provisioningRejectedReason, true
	default:
		return "", false
	}
}

// requireDLQ is the startup preflight for one inbound queue. platform-events'
// DLQPublisher resolves the DLQ from the queue's own RedrivePolicy, so the
// target can never drift from the infra-configured one.
//
//   - A queue with no usable RedrivePolicy is a deployment error: it returns
//     an error and the caller stops startup. Without a DLQ, the library's
//     malformed-message and over-threshold forwards have nowhere to go.
//   - A transient failure (throttling, network) is logged and startup
//     continues. The publisher resolves again on the first forward and
//     caches the result, so it never permanently disables DLQ routing the
//     way a one-shot resolution at startup would.
func requireDLQ(ctx context.Context, dlq events.DLQPublisher, queueURL string, log port.Logger) error {
	rctx, cancel := context.WithTimeout(ctx, dlqStartupTimeout)
	defer cancel()
	dlqURL, err := dlq.ResolveDLQ(rctx, queueURL)
	switch {
	case err == nil:
		if log != nil {
			log.Info("DLQ forwarding wired", map[string]any{"queue_url": queueURL, "dlq_url": dlqURL})
		}
		return nil
	case errors.Is(err, events.ErrDLQNotConfigured), errors.Is(err, events.ErrDLQInvalidRedrivePolicy):
		return fmt.Errorf("queue %s has no usable RedrivePolicy, so dead-lettered messages would be lost: %w", queueURL, err)
	default:
		if log != nil {
			log.Warn("could not resolve DLQ at startup; forwards will resolve it again",
				map[string]any{"queue_url": queueURL, "error": err.Error()})
		}
		return nil
	}
}

// routeRejectsToDLQ forwards a message whose handler returned a permanent
// reject (see dlqReason) straight to its queue's DLQ and acks it, instead of
// letting it burn the full retry budget:
//   - an EVT-15 reject would otherwise be redelivered until SQS redrive, and
//     because the clamp is re-evaluated on every delivery a skewed event
//     could be accepted once wall-clock caught up with it;
//   - a schema violation can never pass, however often it is retried.
//
// What is forwarded is the original transport message (raw body and message
// attributes, from events.SourceMessageFromContext), not a re-serialised
// envelope. That keeps the producer's Glue framing, traceparent and tenant
// attributes, so the DLQ copy can be redriven unchanged. platform-events
// counts the forward in platform_dlq_messages_total itself.
//
// If the source message is unavailable or the DLQ send fails, the original
// error is returned, so the message stays on the queue: the consumer's own
// over-threshold forwarding and SQS redrive remain the backstop.
func routeRejectsToDLQ(h events.Handler, dlq events.DLQPublisher, log port.Logger) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		err := h(ctx, env)
		reason, ok := dlqReason(err)
		if !ok {
			return err
		}
		src, ok := events.SourceMessageFromContext(ctx)
		if !ok {
			return err
		}
		if sErr := dlq.SendToDLQ(ctx, src.QueueURL, src.Body, maps.Clone(src.Attributes), reason); sErr != nil {
			if log != nil {
				log.Warn("DLQ send failed — message stays on the queue for retry and redrive",
					map[string]any{"event_id": env.ID, "event_type": env.Type, "reason": reason, "queue_url": src.QueueURL, "error": sErr.Error()})
			}
			return err
		}
		return nil
	}
}

// RequireDLQ resolves queueURL's DLQ at startup (see requireDLQ).
func RequireDLQ(ctx context.Context, dlq events.DLQPublisher, queueURL string, log port.Logger) error {
	return requireDLQ(ctx, dlq, queueURL, log)
}
