// catalog_plans_consumer.go — dedicated SQS handler for catalog-orgm-plans-q.
//
// Separate from CatalogConsumer (catalog-orgm-q) because this handler needs
// a sysPool-backed TenantRepository for the cross-tenant bulk UPDATE and
// returns meaningful tenant IDs for cache eviction — heavier than the pure
// cache-invalidation CatalogConsumer.
//
// On PlanCatalogChanged the consumer:
//  1. Parses plan_code + feature_set from the event payload.
//  2. Clears om:plans + om:plans:stale (same as CatalogConsumer for belt-and-suspenders).
//  3. Calls TenantRepository.MergeFeatureFlagsForPlan (sysPool / BYPASSRLS):
//     SET feature_flags = $feature_set::jsonb || feature_flags
//     The || operator merges with the RIGHT side winning, so per-tenant overrides
//     are always preserved (T-9 invariant). New plan-baseline keys not present in
//     the tenant's override are added.
//  4. Evicts om:tenant:{id} for every updated tenant (P-1/P-2/I-17 read fresh flags).
//  5. Records the event in processed_events via processOnce (idempotency).
//
// Cache eviction for om:memberships:{tid}:{uid} is intentionally skipped for the
// bulk case: 300 s natural expiry is acceptable for plan-wide flag changes; the
// per-operator O-4 path (single tenant) continues to evict membership caches.
package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	events "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

const catalogPlansConsumerName = "catalog-plans"

// planCatalogChangedPayload is the subset of PlanCatalogChanged that this
// consumer reads. additionalProperties:true on the schema means the event
// may carry extra fields that are intentionally ignored here.
type planCatalogChangedPayload struct {
	PlanCode   string         `json:"plan_code"`
	FeatureSet map[string]any `json:"feature_set"`
}

// PlanCatalogPlansConsumer handles catalog-orgm-plans-q.
type PlanCatalogPlansConsumer struct {
	tenants port.TenantRepository // MUST be sysPool-backed (BYPASSRLS)
	cache   port.Cache
	inbox   port.Inbox
	log     port.SlogStyleLogger
}

// NewPlanCatalogPlansConsumer builds a PlanCatalogPlansConsumer.
// tenants MUST be constructed over the sysPool (BYPASSRLS) so the
// cross-tenant UPDATE is not filtered by RLS.
func NewPlanCatalogPlansConsumer(tenants port.TenantRepository, cache port.Cache, inbox port.Inbox, log port.Logger) *PlanCatalogPlansConsumer {
	return &PlanCatalogPlansConsumer{
		tenants: tenants,
		cache:   cache,
		inbox:   inbox,
		log:     port.NewSlogStyleLogger(log),
	}
}

// Handle implements events.Handler for catalog-orgm-plans-q.
func (c *PlanCatalogPlansConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	if env.Type != eventPlanCatalogChanged {
		return ackUnknown(ctx, c.inbox, c.log, catalogPlansConsumerName, env)
	}

	eventID, err := uuid.Parse(env.ID)
	if err != nil || eventID == uuid.Nil {
		return fmt.Errorf("catalog-plans consumer: invalid event id %q: %w", env.ID, err)
	}

	var payload planCatalogChangedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("catalog-plans consumer: unmarshal payload: %w", err)
	}

	// Validate plan_code against the known plan enum so we don't attempt a
	// BYPASSRLS UPDATE with an arbitrary string.
	plan := domain.TenantPlan(payload.PlanCode)
	switch plan {
	case domain.PlanStarter, domain.PlanPro, domain.PlanEnterprise:
	default:
		// Unknown plan code — ack to avoid DLQ storm; the catalog may be ahead
		// of this service's enum (forward-compat).
		c.log.WarnContext(ctx, "catalog-plans consumer: unknown plan_code — acking without DB write",
			"plan_code", payload.PlanCode, "event_id", env.ID)
		return processOnce(ctx, c.inbox, catalogPlansConsumerName, env, func(context.Context) error { return nil })
	}

	featureSetJSON, err := json.Marshal(payload.FeatureSet)
	if err != nil {
		return fmt.Errorf("catalog-plans consumer: marshal feature_set: %w", err)
	}

	return processOnce(ctx, c.inbox, catalogPlansConsumerName, env, func(txCtx context.Context) error {
		// Step 1 — clear om:plans cache (belt-and-suspenders alongside catalog-orgm-q).
		if delErr := c.cache.Delete(txCtx, omPlansKey, omPlansStaleKey); delErr != nil {
			c.log.WarnContext(txCtx, "catalog-plans consumer: failed to clear om:plans cache — TTL self-heals",
				"event_id", env.ID, "error", delErr)
		}

		// Step 2 — bulk-merge plan feature_set into all active tenants on this plan.
		// Uses SET feature_flags = $feature_set::jsonb || feature_flags so
		// per-tenant overrides win (T-9). Returns the IDs of updated rows.
		updatedIDs, mergeErr := c.tenants.MergeFeatureFlagsForPlan(txCtx, plan, featureSetJSON)
		if mergeErr != nil {
			return fmt.Errorf("catalog-plans consumer: merge feature flags for plan %s: %w", plan, mergeErr)
		}

		// Step 3 — evict om:tenant:{id} for every affected tenant.
		if len(updatedIDs) > 0 {
			keys := make([]string, len(updatedIDs))
			for i, id := range updatedIDs {
				keys[i] = fmt.Sprintf("om:tenant:%s", id)
			}
			if delErr := c.cache.Delete(txCtx, keys...); delErr != nil {
				c.log.WarnContext(txCtx, "catalog-plans consumer: failed to evict tenant caches — TTL self-heals",
					"event_id", env.ID, "tenant_count", len(updatedIDs), "error", delErr)
			}
		}

		metrics.IncCatalogCacheInvalidated()
		c.log.InfoContext(txCtx, "catalog-plans consumer: merged plan feature_set and evicted tenant caches",
			"event_id", env.ID, "plan_code", payload.PlanCode,
			"feature_keys", len(payload.FeatureSet), "tenants_updated", len(updatedIDs))
		return nil
	})
}
