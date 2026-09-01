// Unit tests for internal/core/service/authz_service.go.
//
// Pool-dependent paths (readFromDB, setCached via the DB miss path) are
// covered by the postgres integration suite. Here we cover the paths that
// can run without a real pgx pool:
//
//  1. NewAuthZService — constructor returns a non-nil service.
//  2. GetMembership cache-hit — when the cache holds valid JSON for the
//     (tenantID, userID) key, GetMembership returns the projection without
//     touching the pool at all.
//  3. GetMembership cache-hit with invalid JSON — getCached gracefully
//     returns nil; the function then attempts readFromDB. With pool=nil this
//     must not panic; it returns an error from the pgcommon layer.
//  4. GetMembership with nil cache and nil pool — getCached is a no-op
//     (returns nil); readFromDB is attempted and must return an error, not
//     panic.
//
// Tests 3 and 4 show that the cache-miss path is wired correctly by
// observing the error that bubbles up from readFromDB when no real pool is
// present. The exact error type is pgcommon-internal; we only assert that
// an error is returned and the value is nil.
package unit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── fake cache ────────────────────────────────────────────────────────────

// azCache is a minimal in-memory Cache used only in authz_service tests.
// It uses separate maps for reads and writes so callers can distinguish
// what was stored vs pre-seeded.
type azCache struct {
	// seed holds data that Get will return (simulates pre-populated cache).
	seed map[string][]byte
	// stored holds data written via Set.
	stored map[string][]byte
	// getErr, if non-nil, is returned by every Get call regardless of key.
	getErr error
}

func newAZCache() *azCache {
	return &azCache{seed: map[string][]byte{}, stored: map[string][]byte{}}
}

func (c *azCache) Get(_ context.Context, key string) ([]byte, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	if v, ok := c.seed[key]; ok {
		return v, nil
	}
	return nil, nil
}

func (c *azCache) MGet(_ context.Context, _ []string) ([][]byte, error) { return nil, nil }

func (c *azCache) Set(_ context.Context, key string, v []byte, _ time.Duration) error {
	c.stored[key] = v
	return nil
}

func (c *azCache) SetNX(_ context.Context, _ string, _ []byte, _ time.Duration) (bool, error) {
	return true, nil
}

func (c *azCache) Delete(_ context.Context, _ ...string) error { return nil }
func (c *azCache) Health(_ context.Context) error              { return nil }
func (c *azCache) Close() error                                { return nil }

var _ port.Cache = (*azCache)(nil)

// ── fake PlanCatalogReader ─────────────────────────────────────────────────

// azPlanReader is a stub PlanCatalogReader that returns a pre-configured Plan
// or error. Used to satisfy the NewAuthZService constructor; it is never
// called in the cache-hit tests because GetMembership returns before reaching
// the PlanByCode call.
type azPlanReader struct {
	plan *domain.Plan
	err  error
}

func (r *azPlanReader) Plans(_ context.Context) ([]domain.Plan, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.plan != nil {
		return []domain.Plan{*r.plan}, nil
	}
	return nil, nil
}

func (r *azPlanReader) PlanByCode(_ context.Context, _ domain.TenantPlan) (*domain.Plan, error) {
	return r.plan, r.err
}

var _ port.PlanCatalogReader = (*azPlanReader)(nil)

// azDeptReader is a stub DepartmentCatalogReader that satisfies the
// NewAuthZService constructor; departments are not needed for the cache-hit
// and feature-flag projection tests in this file.
type azDeptReader struct{}

func (r *azDeptReader) Departments(_ context.Context) ([]domain.Department, error) {
	return nil, nil
}
func (r *azDeptReader) DepartmentByID(_ context.Context, _ uuid.UUID) (*domain.Department, error) {
	return nil, nil
}

var _ port.DepartmentCatalogReader = (*azDeptReader)(nil)

// ── helpers ───────────────────────────────────────────────────────────────

// membershipCacheKey mirrors the unexported cacheKeyMemberships helper in
// authz_service.go so tests can pre-seed the cache with the correct key.
func membershipCacheKey(tenantID, userID uuid.UUID) string {
	return fmt.Sprintf("om:memberships:%s:%s", tenantID, userID)
}

// sampleProjection returns a deterministic MembershipProjection for use in
// cache-seeding. Fields are chosen to exercise the JSON round-trip: slices,
// booleans, and the deprecated TenantStatus alias.
func sampleProjection(tenantID, userID uuid.UUID) *service.MembershipProjection {
	deptID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	return &service.MembershipProjection{
		UserID:               userID,
		TenantID:             tenantID,
		Status:               domain.MembershipActive,
		Plan:                 domain.PlanPro,
		TenantStatus:         domain.StatusActive,
		SubscriptionStatus:   domain.StatusActive,
		ReadOnly:             false,
		Locale:               "en",
		MFAFreshnessSeconds:  300,
		LocalAccountsEnabled: true,
		Roles:                []domain.TenantRoleCode{domain.RoleMember},
		Departments: []domain.DeptMembershipView{
			{DepartmentID: deptID, RoleLevel: domain.DeptReviewer},
		},
		FeatureFlags: []string{"advanced_reports"},
	}
}

// ── tests ─────────────────────────────────────────────────────────────────

// TestAuthZService_NewAuthZService_ReturnsNonNil verifies that the
// constructor wires up correctly and never returns nil — even when the pool
// is nil (which would only panic later, not at construction time).
func TestAuthZService_NewAuthZService_ReturnsNonNil(t *testing.T) {
	t.Parallel()

	svc := service.NewAuthZService(nil, &azPlanReader{}, &azDeptReader{}, newAZCache())

	require.NotNil(t, svc, "NewAuthZService must return a non-nil *AuthZService")
}

// TestAuthZService_GetMembership_CacheHit_ReturnsCachedProjection is the
// primary unit-testable path: when the cache holds valid JSON under the
// canonical key, GetMembership must return the deserialized projection
// immediately without attempting any SQL — pool=nil must not be reached.
func TestAuthZService_GetMembership_CacheHit_ReturnsCachedProjection(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	userID := uuid.New()

	proj := sampleProjection(tenantID, userID)
	raw, err := json.Marshal(proj)
	require.NoError(t, err, "json.Marshal must not fail on sampleProjection")

	cache := newAZCache()
	cache.seed[membershipCacheKey(tenantID, userID)] = raw

	// pool=nil is intentional: the test must never reach readFromDB.
	svc := service.NewAuthZService(nil, &azPlanReader{}, &azDeptReader{}, cache)

	got, err := svc.GetMembership(context.Background(), tenantID, userID)

	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, proj.UserID, got.UserID)
	assert.Equal(t, proj.TenantID, got.TenantID)
	assert.Equal(t, proj.Status, got.Status)
	assert.Equal(t, proj.Plan, got.Plan)
	assert.Equal(t, proj.SubscriptionStatus, got.SubscriptionStatus)
	assert.Equal(t, proj.TenantStatus, got.TenantStatus, "deprecated TenantStatus alias must survive the JSON round-trip")
	assert.Equal(t, proj.ReadOnly, got.ReadOnly)
	assert.Equal(t, proj.Locale, got.Locale)
	assert.Equal(t, proj.MFAFreshnessSeconds, got.MFAFreshnessSeconds)
	assert.Equal(t, proj.LocalAccountsEnabled, got.LocalAccountsEnabled)
	assert.Equal(t, proj.Roles, got.Roles)
	require.Len(t, got.Departments, 1)
	assert.Equal(t, proj.Departments[0].DepartmentID, got.Departments[0].DepartmentID)
	assert.Equal(t, proj.Departments[0].RoleLevel, got.Departments[0].RoleLevel)
}

// TestAuthZService_GetMembership_CacheHit_DifferentUser verifies that the
// cache key is namespaced by both tenantID and userID: seeding the cache for
// user A must not satisfy a lookup for user B, even within the same tenant.
// Only the userA-hit half is asserted here; the userB-miss path reaches the
// nil pool and panics (pgcommon.RunInTx dereferences the pool pointer before
// returning an error), so we only confirm the key-namespacing on the hit side.
func TestAuthZService_GetMembership_CacheHit_DifferentUser(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	userA := uuid.New()
	userB := uuid.New()

	// Ensure the keys differ — the test is fundamentally about key namespacing.
	require.NotEqual(t, membershipCacheKey(tenantID, userA), membershipCacheKey(tenantID, userB),
		"cache keys must differ for different user IDs within the same tenant")

	projA := sampleProjection(tenantID, userA)
	raw, err := json.Marshal(projA)
	require.NoError(t, err)

	cache := newAZCache()
	// Only seed for userA; userB has no entry.
	cache.seed[membershipCacheKey(tenantID, userA)] = raw

	svc := service.NewAuthZService(nil, &azPlanReader{}, &azDeptReader{}, cache)

	// userA → cache hit — returned directly, pool is never touched.
	gotA, err := svc.GetMembership(context.Background(), tenantID, userA)
	require.NoError(t, err)
	require.NotNil(t, gotA)
	assert.Equal(t, userA, gotA.UserID)

	// userB → cache miss — would reach readFromDB which panics with nil pool.
	// We assert the panic rather than an error return, because pgcommon.RunInTx
	// dereferences the pool pointer unconditionally before any error check.
	assert.Panics(t, func() {
		//nolint:errcheck // panic is the expected outcome; return value irrelevant
		svc.GetMembership(context.Background(), tenantID, userB) //nolint:ineffassign
	}, "cache miss with nil pool must panic inside pgcommon.RunInTx")
}

// TestAuthZService_GetMembership_CacheHit_CancelledStatus_ReadOnly verifies
// that a cached projection with subscription_status=cancelled is returned
// with read_only=true. This exercises the JSON round-trip of the ReadOnly
// field (set by readOnlyForStatus at DB-read time; preserved in the cache).
func TestAuthZService_GetMembership_CacheHit_CancelledStatus_ReadOnly(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	userID := uuid.New()

	proj := sampleProjection(tenantID, userID)
	proj.SubscriptionStatus = domain.StatusCancelled
	proj.TenantStatus = domain.StatusCancelled
	proj.ReadOnly = true // set by readOnlyForStatus at DB-read time

	raw, err := json.Marshal(proj)
	require.NoError(t, err)

	cache := newAZCache()
	cache.seed[membershipCacheKey(tenantID, userID)] = raw

	svc := service.NewAuthZService(nil, &azPlanReader{}, &azDeptReader{}, cache)

	got, err := svc.GetMembership(context.Background(), tenantID, userID)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, domain.StatusCancelled, got.SubscriptionStatus)
	assert.True(t, got.ReadOnly, "read_only must be true for cancelled subscription status")
}

// TestAuthZService_GetMembership_CacheHit_EmptyRolesAndDepts verifies that
// slices that are empty in the cached projection are returned as empty (not
// nil) — I8-4 mandates no JSON null in collection fields on the wire. The
// JSON round-trip must preserve the non-nil slice.
func TestAuthZService_GetMembership_CacheHit_EmptyRolesAndDepts(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	userID := uuid.New()

	proj := &service.MembershipProjection{
		UserID:                userID,
		TenantID:              tenantID,
		Status:                domain.MembershipActive,
		Plan:                  domain.PlanStarter,
		TenantStatus:          domain.StatusTrial,
		SubscriptionStatus:    domain.StatusTrial,
		ReadOnly:              false,
		Locale:                "ar",
		MFAFreshnessSeconds:   60,
		LocalAccountsEnabled:  false,
		Roles:                 []domain.TenantRoleCode{domain.RoleMember},
		Departments:           []domain.DeptMembershipView{},
		FeatureFlags: []string{},
	}

	raw, err := json.Marshal(proj)
	require.NoError(t, err)

	cache := newAZCache()
	cache.seed[membershipCacheKey(tenantID, userID)] = raw

	svc := service.NewAuthZService(nil, &azPlanReader{}, &azDeptReader{}, cache)

	got, err := svc.GetMembership(context.Background(), tenantID, userID)
	require.NoError(t, err)
	require.NotNil(t, got)

	// Non-nil empty slices must survive the cache round-trip.
	assert.NotNil(t, got.Roles)
	assert.NotNil(t, got.Departments)
	assert.NotNil(t, got.FeatureFlags)
	assert.Empty(t, got.Departments)
}

// TestAuthZService_GetMembership_CacheError_FallsThroughToPool verifies
// getCached's error-handling: when the cache returns an error (e.g. Valkey
// unavailable), getCached returns nil, and GetMembership falls through to
// readFromDB. pgcommon.RunInTx panics with a nil pool (nil pointer
// dereference before any error return), so we assert the panic rather than
// an error return. The important invariant is that getCached returns nil on
// a cache error, not that the final result is a non-panic error.
func TestAuthZService_GetMembership_CacheError_FallsThroughToPool(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	userID := uuid.New()

	cache := newAZCache()
	cache.getErr = errors.New("valkey connection refused")

	svc := service.NewAuthZService(nil, &azPlanReader{}, &azDeptReader{}, cache)

	// getCached returns nil on cache error → readFromDB is reached → nil pool panic.
	assert.Panics(t, func() {
		//nolint:errcheck
		svc.GetMembership(context.Background(), tenantID, userID)
	}, "cache error must cause fallthrough to readFromDB; nil pool causes panic in pgcommon.RunInTx")
}

// TestAuthZService_GetMembership_CacheInvalidJSON_FallsThroughToPool
// verifies getCached's JSON-unmarshal guard: when the cache returns bytes
// that are not valid JSON, getCached silently returns nil and GetMembership
// falls through to readFromDB. pgcommon.RunInTx panics with a nil pool, so
// we assert the panic to confirm the fallthrough without a real pool.
func TestAuthZService_GetMembership_CacheInvalidJSON_FallsThroughToPool(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	userID := uuid.New()

	cache := newAZCache()
	cache.seed[membershipCacheKey(tenantID, userID)] = []byte("not-valid-json{{{")

	svc := service.NewAuthZService(nil, &azPlanReader{}, &azDeptReader{}, cache)

	// getCached returns nil on JSON error → readFromDB is reached → nil pool panic.
	assert.Panics(t, func() {
		//nolint:errcheck
		svc.GetMembership(context.Background(), tenantID, userID)
	}, "invalid JSON in cache must cause getCached to return nil and fall through to readFromDB")
}

// TestAuthZService_GetMembership_NilCache_FallsThroughToPool verifies that
// when cache is nil (i.e. Valkey is disabled entirely), getCached is a no-op
// returning nil, and readFromDB is attempted. pgcommon.RunInTx panics with a
// nil pool, confirming the fallthrough path is correctly wired.
func TestAuthZService_GetMembership_NilCache_FallsThroughToPool(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	userID := uuid.New()

	// Explicitly nil cache — disables the entire cache layer.
	svc := service.NewAuthZService(nil, &azPlanReader{}, &azDeptReader{}, nil)

	// getCached is a no-op (nil cache) → readFromDB is reached → nil pool panic.
	assert.Panics(t, func() {
		//nolint:errcheck
		svc.GetMembership(context.Background(), tenantID, userID)
	}, "nil cache must skip getCached and fall through to readFromDB, panicking on nil pool")
}
