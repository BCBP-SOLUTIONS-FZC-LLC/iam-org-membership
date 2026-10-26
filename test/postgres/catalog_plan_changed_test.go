//go:build integration

// PlanCatalogChanged against a real Postgres (LLD §6.3 / §7.1, rev 2.39):
// the consumer never writes tenants.feature_flags (T-9), records the event in
// processed_events, and retires the tenant's cached I-8 projection so the
// effective flags follow the catalog without waiting for the 300 s TTL.
package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
)

// mapCache is an in-memory port.Cache with real MGET semantics.
type mapCache struct {
	mu   sync.Mutex
	data map[string][]byte
	sets int
}

func newMapCache() *mapCache { return &mapCache{data: map[string][]byte{}} }

func (c *mapCache) Get(_ context.Context, k string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data[k], nil
}

func (c *mapCache) MGet(_ context.Context, keys []string) ([][]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(keys))
	for i, k := range keys {
		out[i] = c.data[k]
	}
	return out, nil
}

func (c *mapCache) Set(_ context.Context, k string, v []byte, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[k] = v
	c.sets++
	return nil
}

func (c *mapCache) SetNX(ctx context.Context, k string, v []byte, ttl time.Duration) (bool, error) {
	return true, c.Set(ctx, k, v, ttl)
}

func (c *mapCache) Delete(_ context.Context, keys ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range keys {
		delete(c.data, k)
	}
	return nil
}

func (c *mapCache) Health(context.Context) error { return nil }
func (c *mapCache) Close() error                 { return nil }

var _ port.Cache = (*mapCache)(nil)

func TestPlanCatalogChanged_FeatureFlagsUntouched_I8FollowsCatalog(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	tenantID, ownerID := seedTenantWithOwner(t, ctx, fx, "plan-catalog-t9")
	// An enterprise tenant with one per-tenant override (the T-9 delta).
	_, err := fx.rawPool.Exec(ctx,
		`UPDATE tenants SET plan = 'enterprise', feature_flags = '{"custom_override": true}'::jsonb WHERE id = $1`, tenantID)
	require.NoError(t, err)
	readTenant := func() (string, int64) {
		var flags string
		var version int64
		require.NoError(t, fx.rawPool.QueryRow(ctx,
			`SELECT feature_flags::text, record_version FROM tenants WHERE id = $1`, tenantID).Scan(&flags, &version))
		return flags, version
	}
	flagsBefore, versionBefore := readTenant()

	cache := newMapCache()
	plans := newFakeCatalogPlans() // enterprise baseline: require_mfa_all_users_allowed
	authz := service.NewAuthZService(pgadapter.NewAuthZRepository(fx.appPool), plans, newFakeCatalogDepartments(), cache)
	tctx := withTenant(ctx, tenantID)

	proj, err := authz.GetMembership(tctx, tenantID, ownerID)
	require.NoError(t, err)
	assert.Equal(t, []string{"custom_override", "require_mfa_all_users_allowed"}, proj.FeatureFlags)

	// Catalog removes the grant from the enterprise baseline (CAT-5).
	plans.mu.Lock()
	ent := plans.rows[domain.PlanEnterprise]
	ent.FeatureSet = map[string]any{}
	plans.rows[domain.PlanEnterprise] = ent
	plans.mu.Unlock()

	proj, err = authz.GetMembership(tctx, tenantID, ownerID)
	require.NoError(t, err)
	assert.Contains(t, proj.FeatureFlags, "require_mfa_all_users_allowed", "precondition: I-8 is served from cache until the event lands")

	c := consumer.NewCatalogConsumer(cache, pgadapter.NewInboxRepository(fx.appPool, &captureOutbox{}), nil)
	env := mkEnvelope(t, "PlanCatalogChanged", uuid.Nil, time.Now().UTC(), map[string]any{
		"code": "enterprise", "change_type": "updated", "feature_set": map[string]any{},
	})
	env.TenantID, env.Subject, env.Source = "", "plans", "iam-catalog-admin"
	require.NoError(t, c.Handle(ctx, env))

	// T-9: the override delta and the row version are untouched.
	flagsAfter, versionAfter := readTenant()
	assert.JSONEq(t, flagsBefore, flagsAfter, "PlanCatalogChanged must not write tenants.feature_flags")
	assert.Equal(t, versionBefore, versionAfter, "no UPDATE of the tenants row at all")

	// The removed grant is gone from I-8 at once; the override survives.
	proj, err = authz.GetMembership(tctx, tenantID, ownerID)
	require.NoError(t, err)
	assert.Equal(t, []string{"custom_override"}, proj.FeatureFlags)

	// Inbox-deduped under consumer="catalog"; a redelivery is a no-op.
	var n int
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT count(*) FROM processed_events WHERE event_id = $1 AND consumer = 'catalog'`, env.ID).Scan(&n))
	assert.Equal(t, 1, n)
	setsBefore := cache.sets
	require.NoError(t, c.Handle(ctx, env))
	assert.Equal(t, setsBefore, cache.sets, "redelivery must not bump the generation again")
	flagsAfter, _ = readTenant()
	assert.JSONEq(t, flagsBefore, flagsAfter)
}
