// Package consumer — CatalogConsumer handles DepartmentCatalogChanged and
// PlanCatalogChanged events from catalog-orgm-q (published by iam-catalog-admin
// on iam-catalog-events). It is the only catalog queue: the separate
// catalog-orgm-plans-q was folded in here in LLD rev 2.39, once the plan
// handler stopped writing tenants.feature_flags (T-9).
//
// DepartmentCatalogChanged clears om:departments (primary + stale) so the next
// CatalogService call fetches fresh data instead of serving the cached catalog
// for up to 10 minutes (Gap-12 / CAT-GAP-004). No processed_events dedup:
// clearing a cache key twice is always safe, so the DB round-trip is
// unnecessary overhead.
//
// PlanCatalogChanged (LLD §6.3 / §7.1, rev 2.39) never writes Postgres. It
//  1. clears om:plans + om:plans:stale, so the next planDefaults lookup reads
//     the new catalog from Catalog's CAT-I2;
//  2. sets om:plans:gen:{plan} to the event id, which retires every cached I-8
//     projection (om:memberships:{tenant}:{user}) of every tenant on that plan:
//     AuthZService compares each entry's stamped generation with the current
//     one and treats a mismatch as a miss (CACHE-11). One SET per plan, however
//     many tenants or members are on it.
//
// The effective flags are planDefaults(plan) ∪ tenants.feature_flags, computed
// at read time (PLAN-6); feature_flags holds only the per-tenant override delta
// (T-9) and is never touched here. Unknown or missing plan code → every plan's
// generation is bumped. Runs through processOnce like the lifecycle consumers;
// a Valkey failure returns an error so SQS redelivers instead of leaving I-8 on
// the old flags until the TTL.
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

const (
	// catalogConsumerName is the processed_events consumer bucket for
	// catalog-orgm-q (PlanCatalogChanged and unknown types).
	catalogConsumerName = "catalog"

	// eventDepartmentCatalogChanged is the event type emitted by
	// iam-catalog-admin whenever a department is created, renamed, or retired.
	eventDepartmentCatalogChanged = "DepartmentCatalogChanged"

	// eventPlanCatalogChanged is the event type emitted by iam-catalog-admin
	// after every CAT-5 plan PATCH (CAT-GAP-004).
	eventPlanCatalogChanged = "PlanCatalogChanged"

	// om:departments cache keys — must match cache_keys.go values exactly.
	omDepartmentsKey      = "om:departments"
	omDepartmentsStaleKey = "om:departments:stale"

	// om:plans cache keys — must match cache_keys.go values exactly.
	omPlansKey      = "om:plans"
	omPlansStaleKey = "om:plans:stale"
)

// planCatalogChangedPayload is the subset of Catalog's PlanCatalogChanged
// payload this consumer reads. The producer field is `code` (iam-catalog-admin
// schemas/plan_catalog_changed.json); feature_set is deliberately ignored —
// planDefaults is always re-read from Catalog, never copied into Core.
type planCatalogChangedPayload struct {
	Code string `json:"code"`
}

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

// NewCatalogConsumer builds a CatalogConsumer. inbox dedups PlanCatalogChanged
// and records unknown event types (ackUnknown).
func NewCatalogConsumer(cache port.Cache, inbox port.Inbox, log port.Logger) *CatalogConsumer {
	return &CatalogConsumer{
		cache: cache,
		inbox: inbox,
		log:   port.NewSlogStyleLogger(log),
	}
}

// Handle implements events.Handler for catalog-orgm-q.
func (c *CatalogConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	switch env.Type {
	case eventDepartmentCatalogChanged, eventPlanCatalogChanged:
	default:
		return ackUnknown(ctx, c.inbox, c.log, catalogConsumerName, env)
	}

	eventID, err := uuid.Parse(env.ID)
	if err != nil || eventID == uuid.Nil {
		return fmt.Errorf("catalog consumer: invalid event id %q: %w", env.ID, err)
	}

	if env.Type == eventPlanCatalogChanged {
		return c.handlePlanChanged(ctx, env)
	}
	c.clearDepartments(ctx, env)
	return nil
}

// clearDepartments clears om:departments + om:departments:stale. A DELETE
// failure is best-effort — the 600 s TTL self-heals.
func (c *CatalogConsumer) clearDepartments(ctx context.Context, env events.Envelope[json.RawMessage]) {
	if err := c.cache.Delete(ctx, omDepartmentsKey, omDepartmentsStaleKey); err != nil {
		c.log.WarnContext(ctx, "catalog consumer: failed to clear cache — will self-heal on TTL expiry",
			"event_type", env.Type, "primary_key", omDepartmentsKey, "event_id", env.ID, "error", err)
		return
	}
	metrics.IncCatalogCacheInvalidated()
	c.log.InfoContext(ctx, "catalog consumer: cleared catalog cache keys",
		"event_type", env.Type, "primary_key", omDepartmentsKey, "stale_key", omDepartmentsStaleKey, "event_id", env.ID)
}

// handlePlanChanged invalidates om:plans and bumps the generation of each
// affected plan. Order matters: om:plans is cleared first, so an I-8 back-fill
// that reads the new generation also reads the new catalog (AuthZService.getCached).
func (c *CatalogConsumer) handlePlanChanged(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	plans := c.affectedPlans(ctx, env)
	return processOnce(ctx, c.inbox, catalogConsumerName, env, func(txCtx context.Context) error {
		if err := c.cache.Delete(txCtx, omPlansKey, omPlansStaleKey); err != nil {
			return fmt.Errorf("catalog consumer: clear om:plans: %w", err)
		}
		// The event id is the new generation: unique per change, and a
		// redelivery writes the same value, so it retires nothing twice.
		for _, p := range plans {
			if err := c.cache.Set(txCtx, port.PlanGenerationKey(p), []byte(env.ID), port.PlanGenerationTTL); err != nil {
				return fmt.Errorf("catalog consumer: bump %s: %w", port.PlanGenerationKey(p), err)
			}
		}
		metrics.IncCatalogCacheInvalidated()
		c.log.InfoContext(txCtx, "catalog consumer: plan catalog changed — cleared om:plans and retired cached I-8 projections",
			"event_id", env.ID, "plans", plans)
		return nil
	})
}

// affectedPlans returns the plan named by the payload's code, or every known
// plan when the code is missing, unparseable or not a tenant_plan value.
func (c *CatalogConsumer) affectedPlans(ctx context.Context, env events.Envelope[json.RawMessage]) []domain.TenantPlan {
	var payload planCatalogChangedPayload
	if len(env.Payload) > 0 {
		_ = json.Unmarshal(env.Payload, &payload)
	}
	for _, p := range domain.KnownPlans() {
		if string(p) == payload.Code {
			return []domain.TenantPlan{p}
		}
	}
	c.log.WarnContext(ctx, "catalog consumer: PlanCatalogChanged without a known plan code — retiring cached I-8 projections for every plan",
		"event_id", env.ID, "code", payload.Code)
	return domain.KnownPlans()
}
