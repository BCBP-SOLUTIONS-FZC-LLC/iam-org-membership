package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	events "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// ── fakes ────────────────────────────────────────────────────────────────────

// plansTenantRepo is a TenantRepository stub that records MergeFeatureFlagsForPlan calls.
type plansTenantRepo struct {
	port.TenantRepositoryNoop
	mergeCalledWith *mergePlansCall
	mergeResult     []uuid.UUID
	mergeErr        error
}

type mergePlansCall struct {
	plan    domain.TenantPlan
	payload []byte
}

func (r *plansTenantRepo) MergeFeatureFlagsForPlan(_ context.Context, plan domain.TenantPlan, featureSet []byte) ([]uuid.UUID, error) {
	r.mergeCalledWith = &mergePlansCall{plan: plan, payload: featureSet}
	return r.mergeResult, r.mergeErr
}

// satisfy port.Cache fully using stubCatalogCache
type plansFullCache struct {
	stubCatalogCache
}

func (c *plansFullCache) Delete(_ context.Context, keys ...string) error {
	copied := append([]string(nil), keys...)
	c.deleted = append(c.deleted, copied)
	return c.err
}

// plansEnv builds a PlanCatalogChanged envelope.
func plansEnv(planCode string, featureSet map[string]any) events.Envelope[json.RawMessage] {
	payload, _ := json.Marshal(planCatalogChangedPayload{
		PlanCode:   planCode,
		FeatureSet: featureSet,
	})
	return events.Envelope[json.RawMessage]{
		ID:      uuid.New().String(),
		Type:    eventPlanCatalogChanged,
		Payload: json.RawMessage(payload),
	}
}

func newPlansConsumer(repo *plansTenantRepo, cache *plansFullCache, dedup *recordingDedup) *PlanCatalogPlansConsumer {
	return NewPlanCatalogPlansConsumer(repo, cache, newFakeInbox(stubTx{}, dedup), nil)
}

// ── tests ─────────────────────────────────────────────────────────────────────

// PCPC-01: happy path — Enterprise plan change merges feature_set and evicts caches.
func TestPlanCatalogPlansConsumer_Handle_Enterprise_MergesAndEvicts(t *testing.T) {
	t.Parallel()
	t1, t2 := uuid.New(), uuid.New()
	repo := &plansTenantRepo{mergeResult: []uuid.UUID{t1, t2}}
	cache := &plansFullCache{}
	dedup := &recordingDedup{}
	c := newPlansConsumer(repo, cache, dedup)

	err := c.Handle(context.Background(), plansEnv("enterprise", map[string]any{"sso_enabled": true}))
	require.NoError(t, err)

	// repo was called with the right plan code and non-empty payload
	require.NotNil(t, repo.mergeCalledWith)
	assert.Equal(t, domain.PlanEnterprise, repo.mergeCalledWith.plan)
	var fs map[string]any
	require.NoError(t, json.Unmarshal(repo.mergeCalledWith.payload, &fs))
	assert.Equal(t, true, fs["sso_enabled"])

	// cache evictions: first call om:plans + om:plans:stale; second = om:tenant:{t1} + om:tenant:{t2}
	require.Len(t, cache.deleted, 2)
	assert.Equal(t, []string{omPlansKey, omPlansStaleKey}, cache.deleted[0])
	tenantKeys := cache.deleted[1]
	assert.Len(t, tenantKeys, 2)

	// processed_events recorded
	require.Len(t, dedup.markCalls, 1)
	assert.Equal(t, catalogPlansConsumerName, dedup.markCalls[0][0])
}

// PCPC-02: starter plan change only evicts om:plans (no tenants on starter → empty update list).
func TestPlanCatalogPlansConsumer_Handle_Starter_NoCacheEvict_WhenNoTenantsUpdated(t *testing.T) {
	t.Parallel()
	repo := &plansTenantRepo{mergeResult: nil} // no tenants on starter
	cache := &plansFullCache{}
	dedup := &recordingDedup{}
	c := newPlansConsumer(repo, cache, dedup)

	err := c.Handle(context.Background(), plansEnv("starter", map[string]any{"workflow_templates": 5}))
	require.NoError(t, err)

	// only om:plans eviction; no tenant eviction batch
	require.Len(t, cache.deleted, 1)
	assert.Equal(t, []string{omPlansKey, omPlansStaleKey}, cache.deleted[0])
	assert.NotNil(t, repo.mergeCalledWith)
	assert.Equal(t, domain.PlanStarter, repo.mergeCalledWith.plan)
}

// PCPC-03: unknown plan_code → ackUnknown, no DB write.
func TestPlanCatalogPlansConsumer_Handle_UnknownPlanCode_AcksWithoutDBWrite(t *testing.T) {
	t.Parallel()
	repo := &plansTenantRepo{}
	cache := &plansFullCache{}
	dedup := &recordingDedup{}
	c := newPlansConsumer(repo, cache, dedup)

	env := plansEnv("ultra_enterprise", map[string]any{"unlimited": true})
	err := c.Handle(context.Background(), env)
	require.NoError(t, err)

	assert.Nil(t, repo.mergeCalledWith, "unknown plan must not trigger DB write")
	require.Len(t, dedup.markCalls, 1) // ackUnknown records in processed_events
}

// PCPC-04: invalid event_id → error (hard reject, stays on queue).
func TestPlanCatalogPlansConsumer_Handle_InvalidEventID_Errors(t *testing.T) {
	t.Parallel()
	c := newPlansConsumer(&plansTenantRepo{}, &plansFullCache{}, &recordingDedup{})
	env := plansEnv("enterprise", map[string]any{"sso_enabled": true})
	env.ID = "not-a-uuid"
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
}

// PCPC-05: malformed JSON payload → error.
func TestPlanCatalogPlansConsumer_Handle_MalformedPayload_Errors(t *testing.T) {
	t.Parallel()
	c := newPlansConsumer(&plansTenantRepo{}, &plansFullCache{}, &recordingDedup{})
	env := events.Envelope[json.RawMessage]{
		ID:      uuid.New().String(),
		Type:    eventPlanCatalogChanged,
		Payload: json.RawMessage(`{bad json`),
	}
	err := c.Handle(context.Background(), env)
	require.Error(t, err)
}

// PCPC-06: DB failure → error returned so SQS retries.
func TestPlanCatalogPlansConsumer_Handle_DBError_ReturnsError(t *testing.T) {
	t.Parallel()
	dbErr := errors.New("connection refused")
	repo := &plansTenantRepo{mergeErr: dbErr}
	c := newPlansConsumer(repo, &plansFullCache{}, &recordingDedup{})

	err := c.Handle(context.Background(), plansEnv("pro", map[string]any{"advanced_reporting": true}))
	require.Error(t, err)
	assert.ErrorIs(t, err, dbErr)
}

// PCPC-07: cache eviction failure → best-effort, no error returned.
func TestPlanCatalogPlansConsumer_Handle_CacheEvictError_ReturnsNil(t *testing.T) {
	t.Parallel()
	repo := &plansTenantRepo{mergeResult: []uuid.UUID{uuid.New()}}
	cache := &plansFullCache{stubCatalogCache: stubCatalogCache{err: errors.New("valkey down")}}
	c := newPlansConsumer(repo, cache, &recordingDedup{})

	err := c.Handle(context.Background(), plansEnv("enterprise", map[string]any{"sso_enabled": true}))
	require.NoError(t, err, "cache failure must not abort — TTL self-heals")
}

// PCPC-08: duplicate message → dedup skips fn, no second DB write.
func TestPlanCatalogPlansConsumer_Handle_Redelivery_Dedup(t *testing.T) {
	t.Parallel()
	repo := &plansTenantRepo{}
	cache := &plansFullCache{}
	dedup := &recordingDedup{processed: map[string]bool{}}
	c := newPlansConsumer(repo, cache, dedup)
	env := plansEnv("enterprise", map[string]any{"sso_enabled": true})

	require.NoError(t, c.Handle(context.Background(), env))
	// Mark as seen manually (first delivery already recorded it via fn)
	firstMark := dedup.markCalls[0]
	dedup.processed[dedup.key(firstMark[0], firstMark[1])] = true
	callCountAfterFirst := len(dedup.markCalls)

	// Second delivery — should be a duplicate, fn not re-run.
	require.NoError(t, c.Handle(context.Background(), env))
	assert.Equal(t, callCountAfterFirst, len(dedup.markCalls), "dedup must prevent second fn call")
}

// PCPC-09: unknown event type → ackUnknown.
func TestPlanCatalogPlansConsumer_Handle_UnknownEventType_AckUnknown(t *testing.T) {
	t.Parallel()
	dedup := &recordingDedup{}
	c := newPlansConsumer(&plansTenantRepo{}, &plansFullCache{}, dedup)

	env := events.Envelope[json.RawMessage]{ID: uuid.New().String(), Type: "UnknownType"}
	require.NoError(t, c.Handle(context.Background(), env))
	require.Len(t, dedup.markCalls, 1)
	assert.Equal(t, catalogPlansConsumerName, dedup.markCalls[0][0])
}

// PCPC-10: wires-contract pin — catalogPlansConsumerName is in ProcessedEventsConsumers.
func TestPlanCatalogPlansConsumer_InProcessedEventsConsumers(t *testing.T) {
	names := ProcessedEventsConsumers()
	assert.Contains(t, names, catalogPlansConsumerName,
		"catalog-plans must be listed so the processed-events-prune job retains its dedup rows")
}
