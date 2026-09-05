// Unit tests closing remaining branch gaps in
// internal/core/service/operator_service.go: NewOperatorService (trivial
// constructor), ReassignOwner (O-7: FindByID error, offboarded-tenant gate,
// invalid-owner-candidate — both the lookup-error and not-active shapes,
// Grant error, ClearOwnerlessSince error, event emission with a live
// publisher vs. no publisher, and the happy-path cache invalidation), and
// ActiveRoleCodes (ListByUser error + happy-path code projection).
package unit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── NewOperatorService ──────────────────────────────────────────────────

func TestNewOperatorService_ReturnsNonNil(t *testing.T) {
	svc := service.NewOperatorService(&invTenantRepo{}, &extRoleRepo{}, &fakeMembershipRepo{}, nil, &arTxRunner{})
	require.NotNil(t, svc)
}

// ── ReassignOwner (O-7) ─────────────────────────────────────────────────

func activeMemberRepoFor(userID uuid.UUID) *fakeMembershipRepo {
	return &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), UserID: userID, Status: domain.MembershipActive}, nil
	}}
}

func TestReassignOwner_TenantFindByIDErrorPropagates(t *testing.T) {
	findErr := errors.New("db down")
	tenants := &invTenantRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
		return nil, findErr
	}}
	svc := service.NewOperatorService(tenants, &extRoleRepo{}, &fakeMembershipRepo{}, nil, &arTxRunner{})
	_, err := svc.ReassignOwner(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, findErr)
}

func TestReassignOwner_OffboardedTenant_Rejected(t *testing.T) {
	tenants := &invTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		return &domain.Tenant{ID: id, Status: domain.StatusOffboarded}, nil
	}}
	svc := service.NewOperatorService(tenants, &extRoleRepo{}, &fakeMembershipRepo{}, nil, &arTxRunner{})
	_, err := svc.ReassignOwner(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrTenantOffboarded)
}

func TestReassignOwner_MembershipLookupErrorRejectedAsInvalidCandidate(t *testing.T) {
	tenants := &invTenantRepo{}
	mem := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, errors.New("not a member")
	}}
	svc := service.NewOperatorService(tenants, &extRoleRepo{}, mem, nil, &arTxRunner{})
	_, err := svc.ReassignOwner(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrInvalidOwnerCandidate)
}

func TestReassignOwner_NewOwnerNotActive_Rejected(t *testing.T) {
	tenants := &invTenantRepo{}
	mem := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipSuspended}, nil
	}}
	svc := service.NewOperatorService(tenants, &extRoleRepo{}, mem, nil, &arTxRunner{})
	_, err := svc.ReassignOwner(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrInvalidOwnerCandidate)
}

func TestReassignOwner_GrantErrorPropagates(t *testing.T) {
	newOwnerID := uuid.New()
	tenants := &invTenantRepo{}
	mem := activeMemberRepoFor(newOwnerID)
	grantErr := errors.New("grant failed")
	roles := &extRoleRepo{grantFn: func(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
		return nil, grantErr
	}}
	svc := service.NewOperatorService(tenants, roles, mem, nil, &arTxRunner{})
	_, err := svc.ReassignOwner(context.Background(), uuid.New(), newOwnerID, uuid.New())
	assert.ErrorIs(t, err, grantErr)
}

func TestReassignOwner_ClearOwnerlessSinceErrorPropagates(t *testing.T) {
	newOwnerID := uuid.New()
	clearErr := errors.New("db down")
	tenants := &invTenantRepo{clearOwnerlessSinceFn: func(context.Context, uuid.UUID) error { return clearErr }}
	mem := activeMemberRepoFor(newOwnerID)
	svc := service.NewOperatorService(tenants, &extRoleRepo{}, mem, nil, &arTxRunner{})
	_, err := svc.ReassignOwner(context.Background(), uuid.New(), newOwnerID, uuid.New())
	assert.ErrorIs(t, err, clearErr)
}

func TestReassignOwner_TxRunnerErrorPropagates(t *testing.T) {
	newOwnerID := uuid.New()
	txErr := errors.New("tx aborted")
	tenants := &invTenantRepo{}
	mem := activeMemberRepoFor(newOwnerID)
	svc := service.NewOperatorService(tenants, &extRoleRepo{}, mem, nil, &passthroughTxRunner{runErr: txErr})
	_, err := svc.ReassignOwner(context.Background(), uuid.New(), newOwnerID, uuid.New())
	assert.ErrorIs(t, err, txErr)
}

func TestReassignOwner_NoEventPublisherInContext_SkipsEmission(t *testing.T) {
	newOwnerID := uuid.New()
	tenants := &invTenantRepo{}
	mem := activeMemberRepoFor(newOwnerID)
	// arTxRunner with pub==nil just calls fn(ctx) directly — no publisher injected.
	svc := service.NewOperatorService(tenants, &extRoleRepo{}, mem, nil, &arTxRunner{})
	got, err := svc.ReassignOwner(context.Background(), uuid.New(), newOwnerID, uuid.New())
	require.NoError(t, err)
	assert.Equal(t, domain.RoleTenantOwner, got.RoleCode)
}

func TestReassignOwner_Success_EmitsEventAndInvalidatesCaches(t *testing.T) {
	tenantID, newOwnerID, actorID := uuid.New(), uuid.New(), uuid.New()
	tenants := &invTenantRepo{}
	mem := activeMemberRepoFor(newOwnerID)
	pub := &arPub{}
	cache := &spyCache{}
	svc := service.NewOperatorService(tenants, &extRoleRepo{}, mem, cache, &arTxRunner{pub: pub})

	got, err := svc.ReassignOwner(context.Background(), tenantID, newOwnerID, actorID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.RoleTenantOwner, got.RoleCode)

	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTenantRoleGranted, pub.events[0].Type)
	payload, ok := pub.events[0].Data.(domain.TenantRoleGrantedPayload)
	require.True(t, ok)
	assert.Equal(t, newOwnerID, payload.UserID)
	assert.Equal(t, actorID, payload.ActorID)

	assert.Contains(t, cache.deleteCalls, "om:tenant:"+tenantID.String())
	assert.Contains(t, cache.deleteCalls, "om:memberships:"+tenantID.String()+":"+newOwnerID.String())
}

func TestReassignOwner_NilCache_IsSafe(t *testing.T) {
	newOwnerID := uuid.New()
	tenants := &invTenantRepo{}
	mem := activeMemberRepoFor(newOwnerID)
	svc := service.NewOperatorService(tenants, &extRoleRepo{}, mem, nil, &arTxRunner{})
	assert.NotPanics(t, func() {
		_, err := svc.ReassignOwner(context.Background(), uuid.New(), newOwnerID, uuid.New())
		require.NoError(t, err)
	})
}

// ── ActiveRoleCodes ─────────────────────────────────────────────────────

func TestActiveRoleCodes_ListByUserErrorPropagates(t *testing.T) {
	listErr := errors.New("db down")
	roles := &extRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
		return nil, listErr
	}}
	svc := service.NewOperatorService(&invTenantRepo{}, roles, &fakeMembershipRepo{}, nil, &arTxRunner{})
	_, err := svc.ActiveRoleCodes(context.Background(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, listErr)
}

func TestActiveRoleCodes_ProjectsRoleCodesFromRoles(t *testing.T) {
	roles := &extRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
		return []domain.TenantRole{
			{RoleCode: domain.RoleTenantOwner},
			{RoleCode: domain.RoleTenantAdmin},
		}, nil
	}}
	svc := service.NewOperatorService(&invTenantRepo{}, roles, &fakeMembershipRepo{}, nil, &arTxRunner{})
	got, err := svc.ActiveRoleCodes(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, []domain.TenantRoleCode{domain.RoleTenantOwner, domain.RoleTenantAdmin}, got)
}

func TestActiveRoleCodes_NoRoles_ReturnsEmptySlice(t *testing.T) {
	roles := &extRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
		return nil, nil
	}}
	svc := service.NewOperatorService(&invTenantRepo{}, roles, &fakeMembershipRepo{}, nil, &arTxRunner{})
	got, err := svc.ActiveRoleCodes(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Empty(t, got)
}

var _ port.TenantRoleRepository = (*extRoleRepo)(nil)
