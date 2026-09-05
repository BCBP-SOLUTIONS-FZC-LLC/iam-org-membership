// Unit tests for AuthZService's I-8 cache helpers (getCached/setCached,
// authz_service.go:194/209). All prior authz_service_test.go tests pass a
// nil cache, so the cache-miss/cache-hit/cache-error/corrupt-JSON branches
// were entirely unexercised — this file fills that gap with a fully
// configurable port.Cache fake.
package unit_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cfgCache is a fully configurable port.Cache fake — Get/Set behavior is
// injectable so tests can drive the cache-hit / cache-miss / cache-error /
// corrupt-payload branches precisely.
type cfgCache struct {
	getFn    func(ctx context.Context, key string) ([]byte, error)
	setFn    func(ctx context.Context, key string, value []byte, ttl time.Duration) error
	setCalls []string
}

func (c *cfgCache) Get(ctx context.Context, key string) ([]byte, error) {
	if c.getFn != nil {
		return c.getFn(ctx, key)
	}
	return nil, nil
}
func (c *cfgCache) MGet(context.Context, []string) ([][]byte, error) { return nil, nil }
func (c *cfgCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	c.setCalls = append(c.setCalls, key)
	if c.setFn != nil {
		return c.setFn(ctx, key, value, ttl)
	}
	return nil
}
func (c *cfgCache) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return true, nil
}
func (c *cfgCache) Delete(context.Context, ...string) error { return nil }
func (c *cfgCache) Health(context.Context) error            { return nil }
func (c *cfgCache) Close() error                            { return nil }

var _ port.Cache = (*cfgCache)(nil)

func authzRow() *port.MembershipProjectionRow {
	return &port.MembershipProjectionRow{
		MembershipStatus:   domain.MembershipActive,
		TenantPlan:         domain.PlanStarter,
		SubscriptionStatus: domain.StatusActive,
	}
}

// ── getCached: cache hit returns projection, skips the repo ────────────

func TestAuthZ_GetMembership_CacheHit_SkipsRepo(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	cached := &service.MembershipProjection{UserID: userID, TenantID: tenantID, Plan: domain.PlanStarter}
	raw, err := json.Marshal(cached)
	require.NoError(t, err)

	repoCalled := false
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		repoCalled = true
		return nil, errors.New("must not be called on cache hit")
	}}
	cache := &cfgCache{getFn: func(_ context.Context, key string) ([]byte, error) {
		assert.Equal(t, "om:memberships:"+tenantID.String()+":"+userID.String(), key)
		return raw, nil
	}}
	svc := service.NewAuthZService(repo, &fakePlanReader{}, &fakeDeptReader{}, cache)

	got, err := svc.GetMembership(context.Background(), tenantID, userID)
	require.NoError(t, err)
	assert.Equal(t, domain.PlanStarter, got.Plan)
	assert.False(t, repoCalled, "cache hit must short-circuit the DB read entirely")
}

// ── getCached: cache.Get error falls through to the repo ───────────────

func TestAuthZ_GetMembership_CacheGetError_FallsThroughToRepo(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return authzRow(), nil
	}}
	cache := &cfgCache{getFn: func(context.Context, string) ([]byte, error) {
		return nil, errors.New("cache unavailable")
	}}
	svc := service.NewAuthZService(repo, &fakePlanReader{}, &fakeDeptReader{}, cache)

	got, err := svc.GetMembership(context.Background(), tenantID, userID)
	require.NoError(t, err, "a Get error must fall through to Postgres, not fail I-8")
	assert.Equal(t, domain.MembershipActive, got.Status)
}

// ── getCached: cache miss (nil, nil) falls through to the repo ─────────

func TestAuthZ_GetMembership_CacheMiss_FallsThroughToRepo(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return authzRow(), nil
	}}
	cache := &cfgCache{} // getFn nil → (nil, nil), a plain miss
	svc := service.NewAuthZService(repo, &fakePlanReader{}, &fakeDeptReader{}, cache)

	got, err := svc.GetMembership(context.Background(), tenantID, userID)
	require.NoError(t, err)
	assert.Equal(t, domain.MembershipActive, got.Status)
}

// ── getCached: corrupt JSON falls through to the repo ───────────────────

func TestAuthZ_GetMembership_CorruptCachedJSON_FallsThroughToRepo(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return authzRow(), nil
	}}
	cache := &cfgCache{getFn: func(context.Context, string) ([]byte, error) {
		return []byte("not-json"), nil
	}}
	svc := service.NewAuthZService(repo, &fakePlanReader{}, &fakeDeptReader{}, cache)

	got, err := svc.GetMembership(context.Background(), tenantID, userID)
	require.NoError(t, err)
	assert.Equal(t, domain.MembershipActive, got.Status)
}

// ── setCached: cache-miss success path populates the cache with jitter ──

func TestAuthZ_GetMembership_CacheMiss_PopulatesCacheOnSuccess(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return authzRow(), nil
	}}
	var gotTTL time.Duration
	cache := &cfgCache{
		setFn: func(_ context.Context, key string, _ []byte, ttl time.Duration) error {
			assert.Equal(t, "om:memberships:"+tenantID.String()+":"+userID.String(), key)
			gotTTL = ttl
			return nil
		},
	}
	svc := service.NewAuthZService(repo, &fakePlanReader{}, &fakeDeptReader{}, cache)

	_, err := svc.GetMembership(context.Background(), tenantID, userID)
	require.NoError(t, err)
	require.Len(t, cache.setCalls, 1, "setCached must populate the cache on a successful cache-miss read")
	// CACHE-4: base 300s +/- 30s jitter.
	assert.True(t, gotTTL >= 270*time.Second && gotTTL <= 330*time.Second, "ttl=%s must be within the +/-30s jitter band", gotTTL)
}

// ── setCached: Set error is swallowed, GetMembership still succeeds ────

func TestAuthZ_GetMembership_CacheSetError_StillReturnsProjection(t *testing.T) {
	repo := &fakeAuthZRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*port.MembershipProjectionRow, error) {
		return authzRow(), nil
	}}
	cache := &cfgCache{setFn: func(context.Context, string, []byte, time.Duration) error {
		return errors.New("cache write failed")
	}}
	svc := service.NewAuthZService(repo, &fakePlanReader{}, &fakeDeptReader{}, cache)

	got, err := svc.GetMembership(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err, "a cache Set failure must never fail the I-8 response")
	assert.Equal(t, domain.MembershipActive, got.Status)
}
