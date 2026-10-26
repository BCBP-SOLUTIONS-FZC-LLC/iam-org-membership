package service

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
)

// CACHE-11 (LLD §6.3, rev 2.39): a cached I-8 projection is served only while
// its stamped plan generation equals om:plans:gen:{plan}. The catalog
// consumer's PlanCatalogChanged handler bumps that key; these tests cover the
// reader side.

func fillI8(t *testing.T, svc *AuthZService, plan domain.TenantPlan) (uuid.UUID, uuid.UUID) {
	t.Helper()
	tenantID, userID := uuid.New(), uuid.New()
	_, gens := svc.getCached(context.Background(), tenantID, userID)
	require.NotNil(t, gens, "a readable cache returns the generations it read")
	svc.setCached(context.Background(), &MembershipProjection{
		TenantID: tenantID, UserID: userID, Plan: plan,
		Status: domain.MembershipActive, FeatureFlags: []string{"dedicated_workflow_queue"},
	}, gens)
	return tenantID, userID
}

func TestI8Cache_PlanGeneration_HitWhileUnchanged(t *testing.T) {
	cache := newInMemCache()
	cache.data[port.PlanGenerationKey(domain.PlanEnterprise)] = []byte("gen-1")
	svc := &AuthZService{cache: cache}
	tenantID, userID := fillI8(t, svc, domain.PlanEnterprise)

	hit, _ := svc.getCached(context.Background(), tenantID, userID)
	require.NotNil(t, hit, "entry stamped with the current generation is a hit")
	assert.Equal(t, []string{"dedicated_workflow_queue"}, hit.FeatureFlags)

	var stored map[string]any
	require.NoError(t, json.Unmarshal(cache.data[cacheKeyMemberships(tenantID, userID)], &stored))
	assert.Equal(t, "gen-1", stored["plan_gen"], "the entry records the generation it was built under")
}

func TestI8Cache_PlanGeneration_BumpRetiresEntriesOnThatPlan(t *testing.T) {
	cache := newInMemCache()
	svc := &AuthZService{cache: cache}
	// Built before any PlanCatalogChanged: no generation key exists yet.
	entTenant, entUser := fillI8(t, svc, domain.PlanEnterprise)
	proTenant, proUser := fillI8(t, svc, domain.PlanPro)

	// PlanCatalogChanged{code: enterprise} lands.
	cache.data[port.PlanGenerationKey(domain.PlanEnterprise)] = []byte("evt-1")

	hit, gens := svc.getCached(context.Background(), entTenant, entUser)
	assert.Nil(t, hit, "an enterprise entry built before the bump must be a miss")
	assert.Equal(t, "evt-1", gens[domain.PlanEnterprise], "the miss carries the new generation for the back-fill")

	hit, _ = svc.getCached(context.Background(), proTenant, proUser)
	assert.NotNil(t, hit, "a pro entry is untouched by an enterprise bump")
}

func TestI8Cache_PlanGeneration_BackfillAfterBumpIsAHit(t *testing.T) {
	cache := newInMemCache()
	cache.data[port.PlanGenerationKey(domain.PlanStarter)] = []byte("evt-1")
	svc := &AuthZService{cache: cache}
	tenantID, userID := fillI8(t, svc, domain.PlanStarter)

	hit, _ := svc.getCached(context.Background(), tenantID, userID)
	assert.NotNil(t, hit)

	// A second change for the same plan retires it again.
	cache.data[port.PlanGenerationKey(domain.PlanStarter)] = []byte("evt-2")
	hit, _ = svc.getCached(context.Background(), tenantID, userID)
	assert.Nil(t, hit)
}

// failingMGetCache errors on MGet; the hot path must fall through and must not
// back-fill an entry whose generation it could not read.
type failingMGetCache struct{ *inMemCache }

func (c failingMGetCache) MGet(context.Context, []string) ([][]byte, error) {
	return nil, errors.New("valkey down")
}

func TestI8Cache_PlanGeneration_UnreadableCacheSkipsBackfill(t *testing.T) {
	inner := newInMemCache()
	svc := &AuthZService{cache: failingMGetCache{inner}}
	tenantID, userID := uuid.New(), uuid.New()

	hit, gens := svc.getCached(context.Background(), tenantID, userID)
	assert.Nil(t, hit)
	assert.Nil(t, gens)

	svc.setCached(context.Background(), &MembershipProjection{TenantID: tenantID, UserID: userID, Plan: domain.PlanPro}, gens)
	assert.Empty(t, inner.data, "no back-fill without a generation read")
}

func TestI8Cache_PlanGeneration_ResponseNeverCarriesPlanGen(t *testing.T) {
	cache := newInMemCache()
	cache.data[port.PlanGenerationKey(domain.PlanPro)] = []byte("evt-9")
	svc := &AuthZService{cache: cache}
	tenantID, userID := fillI8(t, svc, domain.PlanPro)

	hit, _ := svc.getCached(context.Background(), tenantID, userID)
	require.NotNil(t, hit)
	raw, err := json.Marshal(hit)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "plan_gen", "plan_gen is cache bookkeeping, not part of the I-8 response")
}
