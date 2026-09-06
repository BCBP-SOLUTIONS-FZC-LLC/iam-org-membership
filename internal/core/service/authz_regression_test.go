// Regression tests for AuthZ hot-path fixes (B11 + G8). Package-internal so
// readOnlyForStatus (unexported) is reachable. No DB needed for these.
package service

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ─────────────────────────────────────────────────────────────────────────
// G8: readOnlyForStatus narrowed to strict LLD §16 A53 ("iff cancelled").
// ─────────────────────────────────────────────────────────────────────────

func TestG8_ReadOnlyForStatus_OnlyCancelled(t *testing.T) {
	cases := []struct {
		status   domain.SubscriptionStatus
		readOnly bool
		reason   string
	}{
		{domain.StatusTrial, false, "trial tenants can write"},
		{domain.StatusActive, false, "active tenants can write"},
		{domain.StatusPastDue, false, "past_due tenants can still write (grace)"},
		{domain.StatusCancelled, true, "cancelled → read_only per §16 A53"},
		{domain.StatusSuspended, false, "suspended: blocked at KC login, doesn't reach I-8"},
		{domain.StatusTrialExpired, false, "trial_expired: blocked earlier, doesn't reach I-8"},
		{domain.StatusOffboarded, false, "offboarded: tenant row soft-deleted, doesn't reach I-8"},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			assert.Equal(t, tc.readOnly, readOnlyForStatus(tc.status), tc.reason)
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────
// B11: MembershipProjection carries subscription_status + read_only.
// The struct-shape assertion — a compile-time contract check.
// ─────────────────────────────────────────────────────────────────────────

func TestMembershipProjection_FieldsPresent(t *testing.T) {
	// Populate the projection and read back both the deprecated alias and
	// the LLD-canonical field. Both must be present and reflect the same
	// underlying subscription state; ReadOnly must be derived, not stored.
	proj := &MembershipProjection{
		TenantStatus:       domain.StatusCancelled,
		SubscriptionStatus: domain.StatusCancelled,
		ReadOnly:           readOnlyForStatus(domain.StatusCancelled),
	}
	assert.Equal(t, domain.StatusCancelled, proj.SubscriptionStatus)
	assert.Equal(t, domain.StatusCancelled, proj.TenantStatus, "deprecated alias still populated")
	assert.True(t, proj.ReadOnly)
}

func TestMembershipProjection_ReadOnlyFalseForTrial(t *testing.T) {
	proj := &MembershipProjection{
		SubscriptionStatus: domain.StatusTrial,
		ReadOnly:           readOnlyForStatus(domain.StatusTrial),
	}
	assert.False(t, proj.ReadOnly)
}

// ── setCached whitebox tests ─────────────────────────────────────────────────
// setCached is only callable from package service; these whitebox tests cover
// the branches that can't be reached via the public GetMembership API without
// a real pgx pool.

// inMemCache is a minimal port.Cache backed by an in-memory map for whitebox tests.
type inMemCache struct {
	data   map[string][]byte
	setErr error
}

func newInMemCache() *inMemCache {
	return &inMemCache{data: make(map[string][]byte)}
}

func (c *inMemCache) Get(_ context.Context, key string) ([]byte, error) {
	return c.data[key], nil
}
func (c *inMemCache) MGet(_ context.Context, keys []string) ([][]byte, error) {
	out := make([][]byte, len(keys))
	for i, k := range keys {
		out[i] = c.data[k]
	}
	return out, nil
}
func (c *inMemCache) Set(_ context.Context, key string, v []byte, _ time.Duration) error {
	if c.setErr != nil {
		return c.setErr
	}
	c.data[key] = v
	return nil
}
func (c *inMemCache) SetNX(_ context.Context, _ string, _ []byte, _ time.Duration) (bool, error) {
	return true, nil
}
func (c *inMemCache) Delete(_ context.Context, _ ...string) error { return nil }
func (c *inMemCache) Health(_ context.Context) error              { return nil }
func (c *inMemCache) Close() error                                { return nil }

var _ port.Cache = (*inMemCache)(nil)

// TestAuthZService_SetCached_NilCache_NoOp verifies that setCached is a no-op
// when cache == nil (the guard at the start of setCached).
func TestAuthZService_SetCached_NilCache_NoOp(t *testing.T) {
	svc := &AuthZService{cache: nil}
	proj := &MembershipProjection{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
	}
	// If setCached panics, the test fails. No assertion needed — just confirm no panic.
	assert.NotPanics(t, func() {
		svc.setCached(context.Background(), proj)
	})
}

// TestAuthZService_SetCached_NilProj_NoOp verifies that setCached is a no-op
// when proj == nil.
func TestAuthZService_SetCached_NilProj_NoOp(t *testing.T) {
	cache := newInMemCache()
	svc := &AuthZService{cache: cache}
	assert.NotPanics(t, func() {
		svc.setCached(context.Background(), nil)
	})
	assert.Empty(t, cache.data, "no cache writes expected for nil proj")
}

// TestAuthZService_SetCached_ValidProj_WritesToCache verifies that when both
// cache and proj are non-nil, setCached serializes the projection and stores
// it under the correct om:memberships: key (covers the happy path of setCached
// that requires readFromDB, which needs pool, to reach normally).
func TestAuthZService_SetCached_ValidProj_WritesToCache(t *testing.T) {
	cache := newInMemCache()
	svc := &AuthZService{cache: cache}
	tenantID := uuid.New()
	userID := uuid.New()
	proj := &MembershipProjection{
		UserID:   userID,
		TenantID: tenantID,
		Status:   domain.MembershipActive,
		Roles:    []domain.TenantRoleCode{domain.RoleMember},
	}

	svc.setCached(context.Background(), proj)

	expectedKey := cacheKeyMemberships(tenantID, userID)
	stored, ok := cache.data[expectedKey]
	assert.True(t, ok, "setCached must write to the cache under the memberships key")
	assert.NotEmpty(t, stored, "cached bytes must not be empty")
}
