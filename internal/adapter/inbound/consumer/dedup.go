package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// skipDuplicate is the cheap processed_events probe every known-type
// handler runs before opening a write transaction. A hit increments
// platform_duplicate_messages_total (IDEMP-4) and short-circuits.
// platform-events has no processed-events API — Envelope.ID + INSERT
// ON CONFLICT DO NOTHING is the library's documented consumer pattern
// (same as iam-realm-provisioner).
func skipDuplicate(ctx context.Context, dedup port.IdempotencyStore, consumer, eventID string) (bool, error) {
	processed, err := dedup.IsProcessed(ctx, consumer, eventID)
	if err != nil {
		return false, err
	}
	if processed {
		if metrics.DuplicateMessages != nil {
			metrics.DuplicateMessages.WithLabelValues(consumer).Inc()
		}
		return true, nil
	}
	return false, nil
}

// ackUnknown is the forward-compat path: an event type with no wired
// handler is logged, counted, and recorded in processed_events inside a
// RunInTx so redelivery does not storm the same unknown type.
func ackUnknown(ctx context.Context, tx port.TxRunner, dedup port.IdempotencyStore, log port.SlogStyleLogger, consumer string, env events.Envelope[json.RawMessage]) error {
	if metrics.UnknownEventAcknowledged != nil {
		metrics.UnknownEventAcknowledged.WithLabelValues("unknown", env.Type).Inc()
	}
	log.Info("unknown event type — silently acknowledging",
		"event_id", env.ID, "event_type", env.Type)
	return markProcessedInTx(ctx, tx, dedup, consumer, env.ID)
}

// markProcessedInTx records eventID on the caller's TxRunner so the dedup
// insert joins withPool's ambient transaction (or opens its own when the
// handler has no local write to be atomic with).
func markProcessedInTx(ctx context.Context, tx port.TxRunner, dedup port.IdempotencyStore, consumer, eventID string) error {
	return tx.RunInTx(ctx, func(txCtx context.Context) error {
		if err := dedup.MarkProcessed(txCtx, consumer, eventID); err != nil {
			return fmt.Errorf("mark processed: %w", err)
		}
		return nil
	})
}
