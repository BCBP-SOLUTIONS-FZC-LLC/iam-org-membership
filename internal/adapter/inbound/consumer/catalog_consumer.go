// Package consumer — CatalogConsumer handles DepartmentCatalogChanged and
// PlanCatalogChanged events from catalog-orgm-q (published by iam-catalog-admin).
// Its job is to clear the om:departments / om:plans cache keys (primary + stale)
// so the next CatalogService call fetches fresh data instead of serving stale
// entries for up to 10 minutes.
//
// Gap-12 / CAT-GAP-004 fix: active cache invalidation eliminates the 600–660 s
// worst-case propagation delay for both department and plan catalog changes.
// Previously both caches had fixed TTLs with no active invalidation; now every
// DepartmentCatalogChanged and PlanCatalogChanged event immediately clears the
// corresponding primary and stale-if-error keys.
//
// No processed_events dedup: clearing either cache key twice is always safe
// (idempotent by construction), so the DB round-trip is unnecessary overhead
// for these consumers. Unknown event types still record via ackUnknown.
package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	events "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

const (
	// catalogConsumerName is the consumer bucket used by ackUnknown when
	// recording unrecognised event types in processed_events.
	catalogConsumerName = "catalog"

	// eventDepartmentCatalogChanged is the event type emitted by
	// iam-catalog-admin whenever a department is created, renamed, or retired.
	eventDepartmentCatalogChanged = "DepartmentCatalogChanged"

	// eventPlanCatalogChanged is the event type emitted by iam-catalog-admin
	// whenever a plan entitlement is changed (e.g. workflow_template_limit,
	// sso_enabled, audit_query_window_days). CAT-GAP-004.
	eventPlanCatalogChanged = "PlanCatalogChanged"

	// om:departments cache keys — must match cache_keys.go values exactly.
	omDepartmentsKey      = "om:departments"
	omDepartmentsStaleKey = "om:departments:stale"

	// om:plans cache keys — must match cache_keys.go values exactly.
	omPlansKey      = "om:plans"
	omPlansStaleKey = "om:plans:stale"
)

// CatalogConsumer handles catalog-orgm-q messages from iam-catalog-admin.
type CatalogConsumer struct {
	cache port.Cache
	inbox port.Inbox
	log   port.SlogStyleLogger
}

// ProcessedEventsConsumers returns every consumer name this service records
// in processed_events, for the processed-events-prune job.
func ProcessedEventsConsumers() []string {
	return []string{consumerName, catalogConsumerName}
}

// NewCatalogConsumer builds a CatalogConsumer. inbox is used only by
// ackUnknown (unknown event types are recorded in processed_events);
// DepartmentCatalogChanged itself does not dedup — clearing the cache twice
// is always safe and the DB round-trip is unnecessary for a cache-invalidation signal.
func NewCatalogConsumer(cache port.Cache, inbox port.Inbox, log port.Logger) *CatalogConsumer {
	return &CatalogConsumer{
		cache: cache,
		inbox: inbox,
		log:   port.NewSlogStyleLogger(log),
	}
}

// Handle implements events.Handler for catalog-orgm-q.
//
// Processing steps:
//  1. Route by event type: DepartmentCatalogChanged → clear om:departments keys;
//     PlanCatalogChanged → clear om:plans keys; unknown → ackUnknown.
//  2. Parse event_id as UUID; invalid → hard error (stays on queue for retry).
//  3. Clear primary + stale-if-error cache keys from Valkey (best-effort).
func (c *CatalogConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	var primaryKey, staleKey string
	switch env.Type {
	case eventDepartmentCatalogChanged:
		primaryKey, staleKey = omDepartmentsKey, omDepartmentsStaleKey
	case eventPlanCatalogChanged:
		primaryKey, staleKey = omPlansKey, omPlansStaleKey
	default:
		return ackUnknown(ctx, c.inbox, c.log, catalogConsumerName, env)
	}

	eventID, err := uuid.Parse(env.ID)
	if err != nil || eventID == uuid.Nil {
		return fmt.Errorf("catalog consumer: invalid event id %q: %w", env.ID, err)
	}

	// Clear both the primary and stale-if-error cache keys. A DELETE failure
	// is best-effort — the TTL self-heals. Redelivery of the same event is
	// safe: clearing an already-cleared key is a no-op.
	if err := c.cache.Delete(ctx, primaryKey, staleKey); err != nil {
		c.log.WarnContext(ctx, "catalog consumer: failed to clear cache — will self-heal on TTL expiry",
			"event_type", env.Type, "primary_key", primaryKey, "event_id", env.ID, "error", err)
	} else {
		metrics.IncCatalogCacheInvalidated()
		c.log.InfoContext(ctx, "catalog consumer: cleared catalog cache keys",
			"event_type", env.Type, "primary_key", primaryKey, "stale_key", staleKey, "event_id", env.ID)
	}

	return nil
}
