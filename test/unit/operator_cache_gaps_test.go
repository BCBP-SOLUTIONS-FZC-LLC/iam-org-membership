// operator_cache_gaps_test.go covers the remaining operator service branches
// not exercised by existing unit tests:
//
//   - SetFeatureFlags — post-tx FindByID success + cache eviction with
//     active user IDs (lines 87-104, previously only the error paths were hit)
//   - ReassignOwner — happy path through RunInTx: Grant + ClearOwnerlessSince
//   - cache eviction (lines 128-162)
//   - ActiveRoleCodes — error path from ListByUser (line 170-172)
//   - ActiveRoleCodes — happy path (roles returned, codes projected)
package unit_test

import (
	"context"
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

// ── shared stubs ──────────────────────────────────────────────────────────────

// ocTenantRepo is a configurable TenantRepository for operator-cache tests.
// Uses TenantRepositoryNoop so new interface methods don't break compilation.
type ocTenantRepo struct {
	port.TenantRepositoryNoop
	findByIDFn            func(context.Context, uuid.UUID) (*domain.Tenant, error)
	setFeatureFlagsFn     func(context.Context, uuid.UUID, []byte, int64) error
	clearOwnerlessSinceFn func(context.Context, uuid.UUID) error
}

func (r *ocTenantRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if r.findByIDFn != nil {
		return r.findByIDFn(ctx, id)
	}
	return &domain.Tenant{ID: id, Status: domain.StatusActive}, nil
}
func (r *ocTenantRepo) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return r.FindByID(ctx, id)
}
func (r *ocTenantRepo) SetFeatureFlags(ctx context.Context, id uuid.UUID, flags []byte, ver int64) error {
	if r.setFeatureFlagsFn != nil {
		return r.setFeatureFlagsFn(ctx, id, flags, ver)
	}
	return nil
}
func (r *ocTenantRepo) ClearOwnerlessSince(ctx context.Context, id uuid.UUID) error {
	if r.clearOwnerlessSinceFn != nil {
		return r.clearOwnerlessSinceFn(ctx, id)
	}
	return nil
}

var _ port.TenantRepository = (*ocTenantRepo)(nil)

// ocMembershipRepo is a configurable MembershipRepository stub.
type ocMembershipRepo struct {
	listActiveUserIDsFn func(context.Context, uuid.UUID) ([]uuid.UUID, error)
	findByUserIDFn      func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error)
}

func (r *ocMembershipRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return &domain.MembershipListPage{}, nil
}
func (r *ocMembershipRepo) FindByUserID(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error) {
	if r.findByUserIDFn != nil {
		return r.findByUserIDFn(ctx, tenantID, userID)
	}
	return &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID, Status: domain.MembershipActive}, nil
}
func (r *ocMembershipRepo) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *ocMembershipRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *ocMembershipRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil }
func (r *ocMembershipRepo) CountActive(context.Context, uuid.UUID) (int, error)           { return 0, nil }
func (r *ocMembershipRepo) ListActiveUserIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error) {
	if r.listActiveUserIDsFn != nil {
		return r.listActiveUserIDsFn(ctx, tenantID)
	}
	return nil, nil
}

var _ port.MembershipRepository = (*ocMembershipRepo)(nil)

// ocTenantRoleRepo is a minimal TenantRoleRepository stub for operator tests.
type ocTenantRoleRepo struct {
	grantFn      func(context.Context, *domain.TenantRole) (*domain.TenantRole, error)
	listByUserFn func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error)
}

func (r *ocTenantRoleRepo) ListByUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error) {
	if r.listByUserFn != nil {
		return r.listByUserFn(ctx, tenantID, userID)
	}
	return nil, nil
}
func (r *ocTenantRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *ocTenantRoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *ocTenantRoleRepo) Grant(ctx context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	if r.grantFn != nil {
		return r.grantFn(ctx, tr)
	}
	return &domain.TenantRole{ID: uuid.New(), TenantID: tr.TenantID, UserID: tr.UserID, RoleCode: tr.RoleCode}, nil
}
func (r *ocTenantRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (r *ocTenantRoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*ocTenantRoleRepo)(nil)

// ocCache is a spy port.Cache that records Delete calls for operator tests.
type ocCache struct {
	deletedKeys []string
}

func (c *ocCache) Get(_ context.Context, _ string) ([]byte, error) { return nil, nil }
func (c *ocCache) MGet(_ context.Context, keys []string) ([][]byte, error) {
	return make([][]byte, len(keys)), nil
}
func (c *ocCache) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error { return nil }
func (c *ocCache) SetNX(_ context.Context, _ string, _ []byte, _ time.Duration) (bool, error) {
	return true, nil
}
func (c *ocCache) Delete(_ context.Context, keys ...string) error {
	c.deletedKeys = append(c.deletedKeys, keys...)
	return nil
}
func (c *ocCache) Health(_ context.Context) error { return nil }
func (c *ocCache) Close() error                   { return nil }

var _ port.Cache = (*ocCache)(nil)

// ── SetFeatureFlags — cache eviction path ─────────────────────────────────────

// TestOperatorService_SetFeatureFlags_CacheEviction_TenantAndMembershipKeys
// verifies that after a successful SetFeatureFlags, the service evicts
// om:tenant:{id} and one om:memberships:{tid}:{uid} key per active member.
// This covers lines 87-104 in operator_service.go.
func TestOperatorService_SetFeatureFlags_CacheEviction_TenantAndMembershipKeys(t *testing.T) {
	tenantID := uuid.New()
	user1 := uuid.New()
	user2 := uuid.New()

	tenants := &ocTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Status: domain.StatusActive, FeatureFlags: map[string]any{}}, nil
		},
	}
	members := &ocMembershipRepo{
		listActiveUserIDsFn: func(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
			return []uuid.UUID{user1, user2}, nil
		},
	}
	cache := &ocCache{}

	svc := service.NewOperatorService(tenants, nil, members, cache, &passthroughTxRunner{})

	got, err := svc.SetFeatureFlags(context.Background(), tenantID, map[string]any{"sso_enabled": true}, 1)
	require.NoError(t, err)
	require.NotNil(t, got)

	// Tenant key must be evicted.
	tenantKey := "om:tenant:" + tenantID.String()
	assert.Contains(t, cache.deletedKeys, tenantKey,
		"SetFeatureFlags must evict om:tenant:{id} after the update")

	// One membership key per active user must be evicted.
	mem1Key := "om:memberships:" + tenantID.String() + ":" + user1.String()
	mem2Key := "om:memberships:" + tenantID.String() + ":" + user2.String()
	assert.Contains(t, cache.deletedKeys, mem1Key, "user1 membership cache key must be evicted")
	assert.Contains(t, cache.deletedKeys, mem2Key, "user2 membership cache key must be evicted")
}

// TestOperatorService_SetFeatureFlags_NoActiveMembers_OnlyTenantKeyEvicted
// verifies that when ListActiveUserIDs returns an empty slice, only the
// om:tenant key is evicted (the per-user Delete call is skipped entirely).
func TestOperatorService_SetFeatureFlags_NoActiveMembers_OnlyTenantKeyEvicted(t *testing.T) {
	tenantID := uuid.New()

	tenants := &ocTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Status: domain.StatusActive, FeatureFlags: map[string]any{}}, nil
		},
	}
	members := &ocMembershipRepo{
		listActiveUserIDsFn: func(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
			return nil, nil // no active members
		},
	}
	cache := &ocCache{}

	svc := service.NewOperatorService(tenants, nil, members, cache, &passthroughTxRunner{})

	_, err := svc.SetFeatureFlags(context.Background(), tenantID, map[string]any{}, 1)
	require.NoError(t, err)

	tenantKey := "om:tenant:" + tenantID.String()
	assert.Contains(t, cache.deletedKeys, tenantKey)
	assert.Len(t, cache.deletedKeys, 1,
		"with no active members only the tenant key must be evicted")
}

// ── ReassignOwner — happy path ─────────────────────────────────────────────────

// TestOperatorService_ReassignOwner_HappyPath_GrantsRoleAndEvictsCache
// verifies the full ReassignOwner success path: tenant lookup, member check,
// Grant inside RunInTx, ClearOwnerlessSince, and post-tx cache eviction.
// This covers the tx body (lines 128-162) which was only reachable with a
// real pgx pool previously.
func TestOperatorService_ReassignOwner_HappyPath_GrantsRoleAndEvictsCache(t *testing.T) {
	tenantID := uuid.New()
	newOwnerID := uuid.New()
	actorID := uuid.New()
	memID := uuid.New()

	tenants := &ocTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Status: domain.StatusActive}, nil
		},
	}
	members := &ocMembershipRepo{
		findByUserIDFn: func(_ context.Context, _, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{
				ID: memID, TenantID: tenantID, UserID: uid, Status: domain.MembershipActive,
			}, nil
		},
	}
	grantedRole := &domain.TenantRole{
		ID: uuid.New(), TenantID: tenantID, UserID: newOwnerID, RoleCode: domain.RoleTenantOwner,
	}
	roles := &ocTenantRoleRepo{
		grantFn: func(_ context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
			assert.Equal(t, domain.RoleTenantOwner, tr.RoleCode)
			assert.Equal(t, newOwnerID, tr.UserID)
			return grantedRole, nil
		},
	}
	cache := &ocCache{}

	svc := service.NewOperatorService(tenants, roles, members, cache, &passthroughTxRunner{})

	got, err := svc.ReassignOwner(context.Background(), tenantID, newOwnerID, actorID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.RoleTenantOwner, got.RoleCode)

	// Cache eviction: om:tenant and om:memberships for the new owner.
	tenantKey := "om:tenant:" + tenantID.String()
	memKey := "om:memberships:" + tenantID.String() + ":" + newOwnerID.String()
	assert.Contains(t, cache.deletedKeys, tenantKey, "ReassignOwner must evict the tenant cache key")
	assert.Contains(t, cache.deletedKeys, memKey, "ReassignOwner must evict the new owner's membership cache key")
}

// TestOperatorService_ReassignOwner_GrantError_PropagatesFromTx verifies that
// when tenRoles.Grant returns an error inside RunInTx, it propagates from
// ReassignOwner (lines 129-135).
func TestOperatorService_ReassignOwner_GrantError_PropagatesFromTx(t *testing.T) {
	grantErr := errors.New("db_write_error")
	tenants := &ocTenantRepo{}
	members := &ocMembershipRepo{}
	roles := &ocTenantRoleRepo{
		grantFn: func(_ context.Context, _ *domain.TenantRole) (*domain.TenantRole, error) {
			return nil, grantErr
		},
	}

	svc := service.NewOperatorService(tenants, roles, members, nil, &passthroughTxRunner{})
	_, err := svc.ReassignOwner(context.Background(), uuid.New(), uuid.New(), uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, grantErr)
}

// TestOperatorService_ReassignOwner_ClearOwnerlessSinceError_Propagates
// verifies that when ClearOwnerlessSince fails inside RunInTx, the error
// propagates (line 137-139).
func TestOperatorService_ReassignOwner_ClearOwnerlessSinceError_Propagates(t *testing.T) {
	clearErr := errors.New("clear_ownerless_failed")
	tenants := &ocTenantRepo{
		clearOwnerlessSinceFn: func(_ context.Context, _ uuid.UUID) error {
			return clearErr
		},
	}
	members := &ocMembershipRepo{}
	roles := &ocTenantRoleRepo{}

	svc := service.NewOperatorService(tenants, roles, members, nil, &passthroughTxRunner{})
	_, err := svc.ReassignOwner(context.Background(), uuid.New(), uuid.New(), uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, clearErr)
}

// ── ActiveRoleCodes — error path and happy path ────────────────────────────────

// TestOperatorService_ActiveRoleCodes_ListByUserError_Propagates verifies that
// a ListByUser error propagates from ActiveRoleCodes (line 170-172).
func TestOperatorService_ActiveRoleCodes_ListByUserError_Propagates(t *testing.T) {
	listErr := errors.New("roles_db_unavailable")
	roles := &ocTenantRoleRepo{
		listByUserFn: func(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
			return nil, listErr
		},
	}
	svc := service.NewOperatorService(nil, roles, nil, nil, nil)

	_, err := svc.ActiveRoleCodes(context.Background(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, listErr)
}

// TestOperatorService_ActiveRoleCodes_WithRoles_ReturnsCodes verifies the
// happy path: ListByUser returns roles, ActiveRoleCodes projects to codes
// (lines 174-178).
func TestOperatorService_ActiveRoleCodes_WithRoles_ReturnsCodes(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()

	roles := &ocTenantRoleRepo{
		listByUserFn: func(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{
				{RoleCode: domain.RoleTenantOwner},
				{RoleCode: domain.RoleTenantAdmin},
			}, nil
		},
	}
	svc := service.NewOperatorService(nil, roles, nil, nil, nil)

	codes, err := svc.ActiveRoleCodes(context.Background(), tenantID, userID)
	require.NoError(t, err)
	assert.Equal(t, []domain.TenantRoleCode{domain.RoleTenantOwner, domain.RoleTenantAdmin}, codes)
}

// suppress unused time import
var _ = time.Second
