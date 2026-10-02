package consumer

import (
	"context"
	"encoding/json"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// processOnce runs fn exactly once per (consumer, env.ID) through the
// platform-events inbox (port.Inbox): the processed_events claim and fn's
// writes commit in one transaction (IDEMP-1..4). A duplicate skips fn and
// increments iam_org_membership_processed_events_duplicates_total, this
// repo's authoritative signal; the inbox itself records the Tier-1
// platform_duplicate_messages_total{queue,event_type}.
func processOnce(ctx context.Context, inbox port.Inbox, consumer string, env events.Envelope[json.RawMessage], fn func(txCtx context.Context) error) error {
	duplicate, err := inbox.ProcessOnce(ctx, consumer, env.ID, env.Type, fn)
	if err != nil {
		return err
	}
	if duplicate {
		metrics.IncDuplicate(ctx, consumer, env.Type)
	}
	return nil
}

// ackUnknown is the forward-compat path: an event type with no wired
// handler is logged, counted, and recorded in processed_events so
// redelivery does not storm the same unknown type.
func ackUnknown(ctx context.Context, inbox port.Inbox, log port.SlogStyleLogger, consumer string, env events.Envelope[json.RawMessage]) error {
	if metrics.UnknownEventAcknowledged != nil {
		metrics.UnknownEventAcknowledged.WithLabelValues(env.Type).Inc()
	}
	log.Info("unknown event type — silently acknowledging",
		"event_id", env.ID, "event_type", env.Type)
	return processOnce(ctx, inbox, consumer, env, func(context.Context) error { return nil })
}
