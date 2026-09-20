// Package consumer — CatalogConsumer handles DepartmentCatalogChanged events
// from catalog-orgm-q (published by iam-catalog-admin). Its only job is to
// clear the om:departments and om:departments:stale cache keys so the next
// request to CatalogService.Departments() fetches fresh data from the
// Catalog Service instead of serving stale entries for up to 10 minutes.
//
// Gap-12 fix: this consumer eliminates the 660-second worst-case propagation
// delay between a Catalog Service department write and org_membership seeing
// the change. Previously, om:departments had a fixed 600s TTL with no active
// invalidation; now every DepartmentCatalogChanged event immediately clears
// both the primary and stale-if-error keys.
package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	events "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

const (
	// catalogConsumerName is the fixed consumer bucket in processed_events
	// for dedup — matches the composite PK (event_id, consumer).
	catalogConsumerName = "catalog"

	// eventDepartmentCatalogChanged is the event type emitted by
	// iam-catalog-admin whenever a department is created, renamed, or retired.
	eventDepartmentCatalogChanged = "DepartmentCatalogChanged"

	// om:departments cache keys — must match cache_keys.go values exactly.
	omDepartmentsKey      = "om:departments"
	omDepartmentsStaleKey = "om:departments:stale"
)

// CatalogConsumer handles catalog-orgm-q messages from iam-catalog-admin.
type CatalogConsumer struct {
	cache       port.Cache
	idempotency port.IdempotencyStore
	txRunner    port.TxRunner
	log         port.SlogStyleLogger
}

// NewCatalogConsumer builds a CatalogConsumer. txRunner is required so
// skipDuplicate / ackUnknown / markProcessedInTx write processed_events
// through the same platform-events consumer pattern as MembershipEventConsumer
// (Envelope.ID + INSERT ON CONFLICT DO NOTHING; no library processed-events API).
func NewCatalogConsumer(cache port.Cache, idempotency port.IdempotencyStore, txRunner port.TxRunner, log port.Logger) *CatalogConsumer {
	return &CatalogConsumer{
		cache:       cache,
		idempotency: idempotency,
		txRunner:    txRunner,
		log:         port.NewSlogStyleLogger(log),
	}
}

// Handle implements events.Handler for catalog-orgm-q.
//
// Processing steps:
//  1. Reject unknown event types (ackUnknown — log, mark processed, return nil).
//  2. Parse event_id as UUID; invalid → hard error (stays on queue for retry).
//  3. Dedup check via skipDuplicate (processed_events, consumer="catalog").
//  4. Clear om:departments + om:departments:stale from Valkey.
//  5. Mark processed via markProcessedInTx after the cache clear attempt.
func (c *CatalogConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	if env.Type != eventDepartmentCatalogChanged {
		return ackUnknown(ctx, c.txRunner, c.idempotency, c.log, catalogConsumerName, env)
	}

	eventID, err := uuid.Parse(env.ID)
	if err != nil || eventID == uuid.Nil {
		return fmt.Errorf("catalog consumer: invalid event id %q", env.ID)
	}

	already, err := skipDuplicate(ctx, c.idempotency, catalogConsumerName, env.ID)
	if err != nil {
		return fmt.Errorf("catalog consumer: idempotency check for %s: %w", env.ID, err)
	}
	if already {
		c.log.InfoContext(ctx, "catalog consumer: duplicate DepartmentCatalogChanged, skipping", "event_id", env.ID)
		return nil
	}

	// Clear both the primary and stale-if-error cache keys. A cache DELETE
	// failure is best-effort — the 600s TTL still self-heals the stale entry,
	// so we log and continue rather than blocking the event from being marked.
	if err := c.cache.Delete(ctx, omDepartmentsKey, omDepartmentsStaleKey); err != nil {
		c.log.WarnContext(ctx, "catalog consumer: failed to clear om:departments cache — will self-heal on TTL expiry",
			"event_id", env.ID, "error", err)
	} else {
		c.log.InfoContext(ctx, "catalog consumer: cleared om:departments + om:departments:stale caches",
			"event_id", env.ID)
	}

	// Mark processed after cache clear — if we crash here the next redelivery
	// will clear the cache again, which is a safe no-op.
	if err := markProcessedInTx(ctx, c.txRunner, c.idempotency, catalogConsumerName, env.ID); err != nil {
		return fmt.Errorf("catalog consumer: mark processed for %s: %w", env.ID, err)
	}

	return nil
}
