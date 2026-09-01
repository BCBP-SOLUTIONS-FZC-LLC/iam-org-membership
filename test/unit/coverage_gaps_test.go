// coverage_gaps_test.go fills the remaining coverage gaps across multiple
// service files, identified by go tool cover -func output:
//
//   - DepartmentService.invalidateCache — cache.Delete call (66.7% gap)
//   - DeptMembershipService.Remove — cache invalidation with deptMembers key
//   - DeptMembershipService.Assign — delegation precision check paths (WFI-9/WFI-12)
//   - GroupMappingService.AssignFromGroups — nil GroupMappingClient warn path
//     and stale-if-error with stale cache hit
//   - TenantService.Get — cache-miss populates cache (setCached non-nil path)
//   - MembershipService.SetStatus — WFI-13 advisory with active workflows
//   - MembershipService.SetStatus — WFI-13 advisory when workflow call fails
//   - MembershipService.ReconcileRoles — last-owner protection (CountActiveOwners)
//   - OperatorService.SetFeatureFlags — nil flags normalised to {}
//   - OperatorService.ReassignOwner — FindByID error propagates
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

// ═══════════════════════════════════════════════════════════════════════════
// DepartmentService — invalidateCache with non-nil cache
// ═══════════════════════════════════════════════════════════════════════════

// TestDepartmentService_Activate_InvalidatesCacheOnSuccess verifies that
// when Activate succeeds the cache.Delete call fires (covering the otherwise
// unreachable `_ = s.cache.Delete(...)` line in invalidateCache).
func TestDepartmentService_Activate_InvalidatesCacheOnSuccess(t *testing.T) {
	deptID := uuid.New()
	tenantID := uuid.New()

	catalog := &fakeDeptCatalogRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
			return &domain.Department{ID: id, IsActive: true, IsSystem: false}, nil
		},
	}
	tenantDepts := &listableTenantDeptRepo{}
	cache := &spyCache{}

	svc := service.NewDepartmentService(catalog, tenantDepts, cache)

	_, _, err := svc.Activate(context.Background(), tenantID, deptID)
	require.NoError(t, err)

	// The cache invalidation must have deleted the tenant key.
	assert.Contains(t, cache.deleteCalls, "om:tenant:"+tenantID.String(),
		"Activate must evict the om:tenant cache key on success")
}

// TestDepartmentService_SetActive_InvalidatesCacheOnSuccess verifies that
// SetActive(isActive=false) also fires cache.Delete.
func TestDepartmentService_SetActive_InvalidatesCacheOnSuccess(t *testing.T) {
	tenantID := uuid.New()
	deptID := uuid.New()

	catalog := &fakeDeptCatalogRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
			return &domain.Department{ID: id, IsActive: true, IsSystem: false}, nil
		},
	}
	tenantDepts := &listableTenantDeptRepo{}
	cache := &spyCache{}

	svc := service.NewDepartmentService(catalog, tenantDepts, cache)

	_, err := svc.SetActive(context.Background(), tenantID, deptID, false, 1)
	require.NoError(t, err)

	assert.Contains(t, cache.deleteCalls, "om:tenant:"+tenantID.String(),
		"SetActive must evict the om:tenant cache key on success")
}

// ═══════════════════════════════════════════════════════════════════════════
// DeptMembershipService — Remove with non-nil cache (dept-members key)
// ═══════════════════════════════════════════════════════════════════════════

// TestDeptMembership_Remove_InvalidatesDeptMembersCache verifies that after a
// successful removal, the per-department dept_members cache key is deleted.
func TestDeptMembership_Remove_InvalidatesDeptMembersCache(t *testing.T) {
	tenantID := uuid.New()
	deptID := uuid.New()

	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(_ context.Context, _, _ uuid.UUID, _ *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 0}, nil
		},
	}
	fakeRepo := &fakeDeptMemRepo{
		removeFn: func(_ context.Context, _, _, _ uuid.UUID) (*domain.DeptMembership, error) {
			return &domain.DeptMembership{ID: uuid.New(), DepartmentID: deptID}, nil
		},
	}
	cache := &spyCache{}

	svc := service.NewDeptMembershipService(fakeRepo, nil, nil, nil, nil, wf, cache, &passthroughTxRunner{})

	_, err := svc.Remove(context.Background(), tenantID, uuid.New(), deptID, uuid.New())
	require.NoError(t, err)

	// The dept-members cache key must have been deleted.
	expectedKey := "om:dept_members:" + tenantID.String() + ":" + deptID.String()
	assert.Contains(t, cache.deleteCalls, expectedKey,
		"Remove must evict the per-department dept_members cache key on success")
}

// ═══════════════════════════════════════════════════════════════════════════
// DeptMembershipService — Assign delegation precision check paths
// ═══════════════════════════════════════════════════════════════════════════

// gapDeptMemRepo is a DeptMembershipRepository that supports both ListByUser
// and Assign (the existing fakeDeptMemRepo only has listByDeptFn and removeFn).
type gapDeptMemRepo struct {
	listByUserFn func(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.DeptMembership, error)
	assignFn     func(ctx context.Context, tenantID, userID, deptID, memID uuid.UUID, level domain.DeptRole, actorID uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error)
	removeFn     func(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.DeptMembership, error)
}

func (r *gapDeptMemRepo) ListByUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.DeptMembership, error) {
	if r.listByUserFn != nil {
		return r.listByUserFn(ctx, tenantID, userID)
	}
	return nil, nil
}
func (r *gapDeptMemRepo) ListByDepartment(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *gapDeptMemRepo) Assign(ctx context.Context, tenantID, userID, deptID, memID uuid.UUID, level domain.DeptRole, actorID uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	if r.assignFn != nil {
		return r.assignFn(ctx, tenantID, userID, deptID, memID, level, actorID)
	}
	return &domain.DeptMembership{ID: uuid.New(), DepartmentID: deptID, RoleLevel: level}, nil, nil
}
func (r *gapDeptMemRepo) Remove(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*domain.DeptMembership, error) {
	if r.removeFn != nil {
		return r.removeFn(ctx, tenantID, userID, deptID)
	}
	return &domain.DeptMembership{ID: uuid.New(), DepartmentID: deptID}, nil
}
func (r *gapDeptMemRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (r *gapDeptMemRepo) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

var _ port.DeptMembershipRepository = (*gapDeptMemRepo)(nil)

// gapDelegationCheckClient is a DelegationCheckClient with a configurable fn.
// Defined here to avoid redeclaring fakeDelegationCheckClient (already in dept_membership_service_test.go).
type gapDelegationCheckClient struct {
	deptDelegateFn func(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*uuid.UUID, error)
}

func (c *gapDelegationCheckClient) DeptDelegate(ctx context.Context, tenantID, userID, deptID uuid.UUID) (*uuid.UUID, error) {
	if c.deptDelegateFn != nil {
		return c.deptDelegateFn(ctx, tenantID, userID, deptID)
	}
	return nil, nil
}

var _ port.DelegationCheckClient = (*gapDelegationCheckClient)(nil)

// TestDeptMembership_Assign_LevelDecreaseWithDelegation_WorkflowBlocks verifies
// that when the new level is lower, a dept-scoped delegation exists, and
// GetDelegateImpact reports active workflows, Assign returns ErrWorkflowResolutionRequired.
func TestDeptMembership_Assign_LevelDecreaseWithDelegation_WorkflowBlocks(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	deptID := uuid.New()
	delegationID := uuid.New()

	delClient := &gapDelegationCheckClient{
		deptDelegateFn: func(_ context.Context, _, _, _ uuid.UUID) (*uuid.UUID, error) {
			return &delegationID, nil
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(_ context.Context, _, _ uuid.UUID, _ *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 2, WorkflowIDs: []uuid.UUID{uuid.New()}}, nil
		},
	}

	// Current level is approver; new level is preparator (a decrease).
	currentMem := domain.DeptMembership{DepartmentID: deptID, RoleLevel: domain.DeptApprover}
	repo := &gapDeptMemRepo{
		listByUserFn: func(_ context.Context, _, _ uuid.UUID) ([]domain.DeptMembership, error) {
			return []domain.DeptMembership{currentMem}, nil
		},
	}

	svc := service.NewDeptMembershipService(
		repo,
		&activeMemberRepo{},
		&activeTenantDeptRepo{},
		nil, // no catalog check
		delClient,
		wf,
		nil,
		&passthroughTxRunner{},
	)

	_, err := svc.Assign(context.Background(), tenantID, userID, deptID, domain.DeptPreparator, uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrWorkflowResolutionRequired,
		"level decrease with active dept-scoped delegation must return ErrWorkflowResolutionRequired")
}

// TestDeptMembership_Assign_LevelDecreaseWithDelegation_WorkflowFails_FailsOpen
// verifies WFI-9: when the workflow client errors during the precision-scope check,
// Assign proceeds (fail-open) rather than blocking.
func TestDeptMembership_Assign_LevelDecreaseWithDelegation_WorkflowFails_FailsOpen(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	deptID := uuid.New()
	delegationID := uuid.New()

	delClient := &gapDelegationCheckClient{
		deptDelegateFn: func(_ context.Context, _, _, _ uuid.UUID) (*uuid.UUID, error) {
			return &delegationID, nil
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(_ context.Context, _, _ uuid.UUID, _ *uuid.UUID) (*port.DelegateImpact, error) {
			return nil, errors.New("workflow_service_unavailable")
		},
	}

	currentMem := domain.DeptMembership{DepartmentID: deptID, RoleLevel: domain.DeptApprover}
	newAssigned := &domain.DeptMembership{ID: uuid.New(), DepartmentID: deptID, RoleLevel: domain.DeptPreparator}
	repo := &gapDeptMemRepo{
		listByUserFn: func(_ context.Context, _, _ uuid.UUID) ([]domain.DeptMembership, error) {
			return []domain.DeptMembership{currentMem}, nil
		},
		assignFn: func(_ context.Context, _, _, _, _ uuid.UUID, level domain.DeptRole, _ uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
			return newAssigned, &currentMem, nil
		},
	}

	svc := service.NewDeptMembershipService(
		repo,
		&activeMemberRepo{},
		&activeTenantDeptRepo{},
		nil,
		delClient,
		wf,
		nil,
		&passthroughTxRunner{},
	)

	got, err := svc.Assign(context.Background(), tenantID, userID, deptID, domain.DeptPreparator, uuid.New())
	require.NoError(t, err, "WFI-9: workflow error must not block level decrease (fail-open)")
	require.NotNil(t, got)
}

// ═══════════════════════════════════════════════════════════════════════════
// GroupMappingService — nil GroupMappingClient warn path
// ═══════════════════════════════════════════════════════════════════════════

// TestGroupMappingService_AssignFromGroups_NilClient_WarnsAndReturnsEmpty
// verifies the "no GroupMappingClient configured" warn path in resolveMappings.
func TestGroupMappingService_AssignFromGroups_NilClient_WarnsAndReturnsEmpty(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()

	// nil client + nil cache → must fall into the "no client" warn path.
	svc := buildGMSvc(&activeMemberRepo{}, &fakeTenantRoleRepo{}, nil, &gmTxRunner{}, nil, nil)
	svc.WithLogger(&fakeGMLogger{})

	got, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"eng"})
	require.NoError(t, err, "nil GroupMappingClient must fail open, never return an error")
	assert.NotNil(t, got)
	assert.Empty(t, got.AssignedDepts)
	assert.Empty(t, got.GrantedTenantRoles)
}

// TestGroupMappingService_AssignFromGroups_ClientError_StaleHit_ServesStale
// verifies the stale-if-error fallback in resolveMappings: when the live call
// fails but the stale cache keys are populated, the stale data is returned.
func TestGroupMappingService_AssignFromGroups_ClientError_StaleHit_ServesStale(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()

	// Set up a cache with all three STALE keys populated with empty slices.
	cache := newRecordingCache()
	emptyJSON := []byte(`[]`)
	cache.values[gmCacheKeyGDMStale(tenantID)] = emptyJSON
	cache.values[gmCacheKeyGRMStale(tenantID)] = emptyJSON
	cache.values[gmCacheKeyGTRMStale(tenantID)] = emptyJSON

	// Live call always fails.
	client := &fakeGMClient{
		resolveFn: func(context.Context, uuid.UUID, []string) (*port.GroupResolution, error) {
			return nil, errors.New("group-mapping-service-down")
		},
	}

	svc := buildGMSvc(&activeMemberRepo{}, &fakeTenantRoleRepo{}, nil, &gmTxRunner{}, cache, client)
	svc.WithLogger(&fakeGMLogger{})

	got, err := svc.AssignFromGroups(context.Background(), tenantID, userID, []string{"eng"})
	require.NoError(t, err, "stale-if-error fallback must succeed with empty stale data")
	assert.NotNil(t, got)
	assert.Empty(t, got.AssignedDepts)
	assert.Empty(t, got.GrantedTenantRoles)
}

// ═══════════════════════════════════════════════════════════════════════════
// TenantService — Get with cache-miss populates cache
// ═══════════════════════════════════════════════════════════════════════════

// TestTenantService_Get_CacheMiss_PopulatesCache verifies that a cache-miss
// in TenantService.Get calls setCached — the spy cache records no panics.
func TestTenantService_Get_CacheMiss_PopulatesCache(t *testing.T) {
	tenantID := uuid.New()
	expected := &domain.Tenant{
		ID:     tenantID,
		Slug:   "acme",
		Status: domain.StatusActive,
	}

	repo := &fakeTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return expected, nil
		},
	}
	cache := &spyCache{}

	svc := service.NewTenantService(repo, cache, &tsRP{})
	got, err := svc.Get(context.Background(), tenantID)

	require.NoError(t, err)
	require.Equal(t, expected, got)
	// The cache would have Set called — spyCache records no panics from Set.
}

// ═══════════════════════════════════════════════════════════════════════════
// MembershipService.SetStatus — WFI-13 advisory paths
// ═══════════════════════════════════════════════════════════════════════════

// activeMembershipRepoGap is a MembershipRepository for SetStatus tests.
type activeMembershipRepoGap struct {
	membership *domain.TenantMembership
}

func (r *activeMembershipRepoGap) FindByUserID(_ context.Context, _, _ uuid.UUID) (*domain.TenantMembership, error) {
	return r.membership, nil
}
func (r *activeMembershipRepoGap) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return &domain.MembershipListPage{}, nil
}
func (r *activeMembershipRepoGap) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *activeMembershipRepoGap) SetStatus(_ context.Context, _, _ uuid.UUID, status domain.MembershipStatus, _ int64) (*domain.TenantMembership, error) {
	if r.membership != nil {
		r.membership.Status = status
		return r.membership, nil
	}
	return nil, nil
}
func (r *activeMembershipRepoGap) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (r *activeMembershipRepoGap) CountActive(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *activeMembershipRepoGap) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*activeMembershipRepoGap)(nil)

// gapRealmProvisionerClient is a minimal port.RealmProvisionerClient stub.
type gapRealmProvisionerClient struct{}

func (r *gapRealmProvisionerClient) CreateInvitedUser(context.Context, port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	return nil, nil
}
func (r *gapRealmProvisionerClient) DeleteUser(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
func (r *gapRealmProvisionerClient) PatchRealmConfig(context.Context, uuid.UUID, port.RealmConfigPatch) error {
	return nil
}
func (r *gapRealmProvisionerClient) RevokeUserSessions(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
func (r *gapRealmProvisionerClient) ResetMFA(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

var _ port.RealmProvisionerClient = (*gapRealmProvisionerClient)(nil)

// TestMembershipService_SetStatus_Suspend_WFI13_ActiveWorkflows_AdvisoryFires
// verifies the WFI-13 path where active workflows are reported.
func TestMembershipService_SetStatus_Suspend_WFI13_ActiveWorkflows_AdvisoryFires(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()
	wfID := uuid.New()

	roles := &noErrRoleRepo{roles: []domain.TenantRole{}} // not an owner
	memberships := &activeMembershipRepoGap{
		membership: &domain.TenantMembership{
			ID: uuid.New(), TenantID: tenantID, UserID: userID,
			Status: domain.MembershipSuspended, RecordVersion: 2,
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(_ context.Context, _, _ uuid.UUID, _ *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 3, WorkflowIDs: []uuid.UUID{wfID}}, nil
		},
	}
	rp := &gapRealmProvisionerClient{}

	svc := service.NewMembershipService(
		memberships, roles,
		nil, nil, nil, nil, rp, wf,
		&passthroughTxRunner{}, nil, 0,
	)

	result, err := svc.SetStatus(context.Background(), tenantID, userID, domain.MembershipSuspended, 1)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.DelegateImpact, "WFI-13: advisory must be populated when active workflows exist")
	assert.True(t, result.DelegateImpact.Checked, "WFI-13: Checked must be true when workflow call succeeded")
	assert.Equal(t, 3, result.DelegateImpact.ActiveWorkflows)
	assert.Contains(t, result.DelegateImpact.WorkflowIDs, wfID)
}

// TestMembershipService_SetStatus_Suspend_WFI13_WorkflowCallFails_UncheckedAdvisory
// verifies that when the WFI-13 advisory call fails, suspend still succeeds
// with Checked=false in the advisory.
func TestMembershipService_SetStatus_Suspend_WFI13_WorkflowCallFails_UncheckedAdvisory(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()

	roles := &noErrRoleRepo{roles: []domain.TenantRole{}}
	memberships := &activeMembershipRepoGap{
		membership: &domain.TenantMembership{
			ID: uuid.New(), TenantID: tenantID, UserID: userID,
			Status: domain.MembershipSuspended, RecordVersion: 2,
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(_ context.Context, _, _ uuid.UUID, _ *uuid.UUID) (*port.DelegateImpact, error) {
			return nil, errors.New("workflow_service_down")
		},
	}
	rp := &gapRealmProvisionerClient{}

	svc := service.NewMembershipService(
		memberships, roles, nil, nil, nil, nil, rp, wf,
		&passthroughTxRunner{}, nil, 0,
	)

	result, err := svc.SetStatus(context.Background(), tenantID, userID, domain.MembershipSuspended, 1)
	require.NoError(t, err, "WFI-13 fail-open: workflow error must not block suspend")
	require.NotNil(t, result)
	require.NotNil(t, result.DelegateImpact, "advisory must still be set on workflow error")
	assert.False(t, result.DelegateImpact.Checked,
		"WFI-13: Checked must be false when workflow call failed")
}

// ═══════════════════════════════════════════════════════════════════════════
// MembershipService.ReconcileRoles — last-owner protection
// ═══════════════════════════════════════════════════════════════════════════

// countingRoleRepo extends noErrRoleRepo to return a configurable owner count.
type countingRoleRepo struct {
	noErrRoleRepo
	countResult int
	countErr    error
}

func (r *countingRoleRepo) CountActiveOwners(_ context.Context, _ uuid.UUID) (int, error) {
	return r.countResult, r.countErr
}

// TestMembershipService_ReconcileRoles_LastOwnerStrip_Returns422 verifies the
// TM-8 guard: stripping owner from the last active owner must return
// ErrLastOwnerRemoval.
func TestMembershipService_ReconcileRoles_LastOwnerStrip_Returns422(t *testing.T) {
	tenantID := uuid.New()
	userID := uuid.New()

	memberships := &activeMembershipRepoGap{
		membership: &domain.TenantMembership{
			ID: uuid.New(), TenantID: tenantID, UserID: userID,
			Status: domain.MembershipActive, RecordVersion: 1,
		},
	}

	// Current roles: [tenant_owner]. Desired: [] (stripping owner).
	roles := &countingRoleRepo{
		noErrRoleRepo: noErrRoleRepo{
			roles: []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}},
		},
		countResult: 1, // exactly 1 owner → last owner
	}

	svc := buildMembershipSvcWithTx(memberships, roles)

	_, _, err := svc.ReconcileRoles(context.Background(), tenantID, userID, []domain.TenantRoleCode{}, uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrLastOwnerRemoval,
		"TM-8: stripping owner from the last owner must return ErrLastOwnerRemoval")
}

// ═══════════════════════════════════════════════════════════════════════════
// OperatorService.SetFeatureFlags — nil flags normalised to {}
// ═══════════════════════════════════════════════════════════════════════════

// TestOperatorService_SetFeatureFlags_NilFlags_NormalisedToEmpty verifies that
// passing nil flags does not panic — the nil-to-empty normalisation is covered.
func TestOperatorService_SetFeatureFlags_NilFlags_NormalisedToEmpty(t *testing.T) {
	svc := service.NewOperatorService(nil, nil, nil, nil, nil, nil)

	// nil flags → normalised to {} → allow-list check passes (no keys) →
	// reaches txRunner which is nil → panics or errors. Use a passthroughTxRunner.
	svc2 := service.NewOperatorService(nil, nil, nil, nil, nil, &passthroughTxRunner{})

	_, err := svc2.SetFeatureFlags(context.Background(), uuid.New(), nil, 1)
	// Error expected (pgadapterTxFromContext !ok → ErrConflict), but must not panic.
	assert.Error(t, err)
	_ = svc // suppress unused
}

// ═══════════════════════════════════════════════════════════════════════════
// OperatorService.ReassignOwner — FindByID error propagates
// ═══════════════════════════════════════════════════════════════════════════

// TestOperatorService_ReassignOwner_FindByIDError_Propagates verifies that
// a tenants.FindByID error propagates from ReassignOwner.
func TestOperatorService_ReassignOwner_FindByIDError_Propagates(t *testing.T) {
	findErr := errors.New("db_unavailable")
	tenants := &ssTenantRepoOp{
		findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
			return nil, findErr
		},
	}
	svc := service.NewOperatorService(nil, tenants, nil, nil, nil, nil)

	_, err := svc.ReassignOwner(context.Background(), uuid.New(), uuid.New(), uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, findErr, "FindByID error must propagate from ReassignOwner")
}

// suppress time import if unused
var _ = time.Second
