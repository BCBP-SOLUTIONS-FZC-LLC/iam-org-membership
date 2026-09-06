// svcgap3_coverage_test.go — third batch covering event-emission blocks and
// ReconcileRoles tx branches. All stubs use "sg3_" prefix.
package unit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── sg3PubTxRunner injects an EventPublisher into the tx context ─────────

type sg3PubTxRunner struct {
	pub port.EventPublisher
}

func (r *sg3PubTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	if r.pub != nil {
		ctx = port.WithEventPublisher(ctx, r.pub)
	}
	return fn(ctx)
}

var _ port.TxRunner = (*sg3PubTxRunner)(nil)

// sg3RecPub records all enqueued events.
type sg3RecPub struct {
	events []*domain.DomainEvent
}

func (p *sg3RecPub) Enqueue(_ context.Context, e *domain.DomainEvent) error {
	p.events = append(p.events, e)
	return nil
}

// ═══════════════════════════════════════════════════════════════════════════
// provisioning_service.go — TrialSignup event emission (lines 232-259)
// ═══════════════════════════════════════════════════════════════════════════

// sg3ProvTenants is a TenantRepository for TrialSignup event-emission tests.
type sg3ProvTenants struct {
	port.TenantRepositoryNoop
}

func (r *sg3ProvTenants) Insert(_ context.Context, t *domain.Tenant) (*domain.Tenant, bool, error) {
	return t, true, nil // freshInsert=true → triggers full seeding path
}

var _ port.TenantRepository = (*sg3ProvTenants)(nil)

// sg3ProvDepts is a TenantDepartmentRepository for TrialSignup with no depts to activate.
type sg3ProvDepts struct{}

func (r *sg3ProvDepts) Find(_ context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	return nil, nil
}
func (r *sg3ProvDepts) List(_ context.Context, _ uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *sg3ProvDepts) ListActive(_ context.Context, _ uuid.UUID) ([]domain.TenantDepartment, error) {
	return nil, nil
}
func (r *sg3ProvDepts) Activate(_ context.Context, tid, did uuid.UUID) (*domain.TenantDepartment, error) {
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did}, nil
}
func (r *sg3ProvDepts) SetActive(_ context.Context, tid, did uuid.UUID, active bool, _ int64) (*domain.TenantDepartment, error) {
	return &domain.TenantDepartment{TenantID: tid, DepartmentID: did, IsActive: active}, nil
}

var _ port.TenantDepartmentRepository = (*sg3ProvDepts)(nil)

// sg3ProvMemberships for TrialSignup Insert.
type sg3ProvMemberships struct{}

func (r *sg3ProvMemberships) List(_ context.Context, _ uuid.UUID, _ *domain.MembershipListCursor, _ int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (r *sg3ProvMemberships) FindByUserID(_ context.Context, _ uuid.UUID, _ uuid.UUID) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *sg3ProvMemberships) Insert(_ context.Context, m *domain.TenantMembership) (*domain.TenantMembership, error) {
	m.ID = uuid.New()
	return m, nil
}
func (r *sg3ProvMemberships) SetStatus(_ context.Context, _ uuid.UUID, _ uuid.UUID, s domain.MembershipStatus, _ int64) (*domain.TenantMembership, error) {
	return &domain.TenantMembership{Status: s}, nil
}
func (r *sg3ProvMemberships) SoftDelete(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ int64) error {
	return nil
}
func (r *sg3ProvMemberships) CountActive(_ context.Context, _ uuid.UUID) (int, error) { return 0, nil }
func (r *sg3ProvMemberships) ListActiveUserIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*sg3ProvMemberships)(nil)

// sg3ProvRoles for TrialSignup Grant.
type sg3ProvRoles struct{ ssRoleRepoDM }

func (r *sg3ProvRoles) Grant(_ context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	return tr, nil
}

var _ port.TenantRoleRepository = (*sg3ProvRoles)(nil)

// sg3ProvLabels for TrialSignup Seed.
type sg3ProvLabels struct{}

func (r *sg3ProvLabels) Seed(_ context.Context, _ uuid.UUID) ([]domain.DeptRoleLabel, error) {
	return nil, nil
}
func (r *sg3ProvLabels) List(_ context.Context, _ uuid.UUID) ([]domain.DeptRoleLabel, error) {
	return nil, nil
}
func (r *sg3ProvLabels) Update(_ context.Context, _ uuid.UUID, _ domain.DeptRole, _ string, _ int64) (*domain.DeptRoleLabel, error) {
	return nil, nil
}

var _ port.DeptRoleLabelRepository = (*sg3ProvLabels)(nil)

// TestProvisioningService_TrialSignup_EmitsEvents covers the pub != nil event
// emission block (lines 232-259): TenantCreated + TrialStarted + TenantRoleGranted.
func TestProvisioningService_TrialSignup_EmitsEvents(t *testing.T) {
	tenantID := uuid.New()
	ownerID := uuid.New()
	pub := &sg3RecPub{}
	txRunner := &sg3PubTxRunner{pub: pub}

	// No system depts to activate (empty dept catalog), so only the 3 events fire.
	svc := service.NewProvisioningService(
		&sg3ProvTenants{},
		&sg3ProvMemberships{},
		&sg3ProvRoles{},
		nil,
		&sg3ProvLabels{},
		&sg3ProvDepts{},
		&ssDeptReader{departmentsFn: func(context.Context) ([]domain.Department, error) { return nil, nil }},
		&ssPlanReader{},
		txRunner, nil, nil,
	)

	_, wasCreated, err := svc.TrialSignup(context.Background(), service.TrialSignupInput{
		TenantID: tenantID, Slug: "acme", Name: "Acme Inc",
		Plan: domain.PlanStarter, OwnerUserID: ownerID,
	})
	require.NoError(t, err)
	assert.True(t, wasCreated)

	types := make([]string, 0, len(pub.events))
	for _, e := range pub.events {
		types = append(types, e.Type)
	}
	assert.Contains(t, types, domain.EventTenantCreated, "TenantCreated event must be emitted")
	assert.Contains(t, types, domain.EventTrialStarted, "TrialStarted event must be emitted")
	assert.Contains(t, types, domain.EventTenantRoleGranted, "TenantRoleGranted event must be emitted")
}

// ═══════════════════════════════════════════════════════════════════════════
// provisioning_service.go — DeleteMember event emission (lines 368-413)
// ═══════════════════════════════════════════════════════════════════════════

// TestProvisioningService_DeleteMember_EmitsRoleAndMembershipRevokedEvents
// covers lines 368-413: pub != nil branches for TenantRoleRevoked,
// DepartmentMembershipRevoked, and MembershipRevoked.
func TestProvisioningService_DeleteMember_EmitsRoleAndMembershipRevokedEvents(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	pub := &sg3RecPub{}
	txRunner := &sg3PubTxRunner{pub: pub}

	mem := &ssMembershipRepoDM{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	roles := &ssRoleRepoDM{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil // non-owner
		},
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
		},
	}
	deptMems := &ssDeptMemRepoDM{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return []domain.DeptMembership{{ID: uuid.New(), DepartmentID: uuid.New()}}, nil
		},
	}

	svc := service.NewProvisioningService(
		&port.TenantRepositoryNoop{}, mem, roles, deptMems,
		nil, nil, nil, nil, txRunner, nil, nil,
	)

	err := svc.DeleteMember(context.Background(), tenantID, userID)
	require.NoError(t, err)

	types := make([]string, 0, len(pub.events))
	for _, e := range pub.events {
		types = append(types, e.Type)
	}
	assert.Contains(t, types, domain.EventTenantRoleRevoked, "TenantRoleRevoked must be emitted")
	assert.Contains(t, types, domain.EventDepartmentMembershipRevoked, "DepartmentMembershipRevoked must be emitted")
	assert.Contains(t, types, domain.EventMembershipRevoked, "MembershipRevoked must be emitted")
}

// ═══════════════════════════════════════════════════════════════════════════
// membership_service.go — ReconcileRoles tx inner branches (lines 312-376)
// ═══════════════════════════════════════════════════════════════════════════

// sg3FullRoleRepo is a TenantRoleRepository with all methods configurable.
type sg3FullRoleRepo struct {
	listByUserFn        func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error)
	countActiveOwnersFn func(context.Context, uuid.UUID) (int, error)
	grantFn             func(context.Context, *domain.TenantRole) (*domain.TenantRole, error)
	revokeFn            func(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error)
}

func (r *sg3FullRoleRepo) ListByUser(ctx context.Context, tid, uid uuid.UUID) ([]domain.TenantRole, error) {
	if r.listByUserFn != nil {
		return r.listByUserFn(ctx, tid, uid)
	}
	return nil, nil
}
func (r *sg3FullRoleRepo) ListByRole(_ context.Context, _ uuid.UUID, _ domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *sg3FullRoleRepo) CountActiveOwners(ctx context.Context, tid uuid.UUID) (int, error) {
	if r.countActiveOwnersFn != nil {
		return r.countActiveOwnersFn(ctx, tid)
	}
	return 2, nil // safe default: multiple owners
}
func (r *sg3FullRoleRepo) Grant(ctx context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
	if r.grantFn != nil {
		return r.grantFn(ctx, tr)
	}
	return tr, nil
}
func (r *sg3FullRoleRepo) Revoke(ctx context.Context, tid, uid uuid.UUID, code domain.TenantRoleCode) (*domain.TenantRole, error) {
	if r.revokeFn != nil {
		return r.revokeFn(ctx, tid, uid, code)
	}
	return &domain.TenantRole{RoleCode: code}, nil
}
func (r *sg3FullRoleRepo) SoftDeleteAllForUser(_ context.Context, _ uuid.UUID, _ uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*sg3FullRoleRepo)(nil)

// sg3MemRepo is a MembershipRepository stub with configurable FindByUserID.
type sg3MemRepo struct {
	findFn func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error)
}

func (r *sg3MemRepo) List(_ context.Context, _ uuid.UUID, _ *domain.MembershipListCursor, _ int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (r *sg3MemRepo) FindByUserID(ctx context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
	if r.findFn != nil {
		return r.findFn(ctx, tid, uid)
	}
	return &domain.TenantMembership{ID: uuid.New()}, nil
}
func (r *sg3MemRepo) Insert(_ context.Context, m *domain.TenantMembership) (*domain.TenantMembership, error) {
	return m, nil
}
func (r *sg3MemRepo) SetStatus(_ context.Context, _ uuid.UUID, _ uuid.UUID, s domain.MembershipStatus, _ int64) (*domain.TenantMembership, error) {
	return &domain.TenantMembership{Status: s}, nil
}
func (r *sg3MemRepo) SoftDelete(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ int64) error {
	return nil
}
func (r *sg3MemRepo) CountActive(_ context.Context, _ uuid.UUID) (int, error) { return 0, nil }
func (r *sg3MemRepo) ListActiveUserIDs(_ context.Context, _ uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*sg3MemRepo)(nil)

// buildReconcileRolesSvc builds a MembershipService for ReconcileRoles tests.
func buildReconcileRolesSvc(
	mem port.MembershipRepository,
	roles port.TenantRoleRepository,
	tenants port.TenantRepository,
	txRunner port.TxRunner,
	rp port.RealmProvisionerClient,
) *service.MembershipService {
	return service.NewMembershipService(mem, roles, nil, tenants, nil, nil, rp, nil, txRunner, nil, 30)
}

// TestReconcileRoles_GrantEmitsEvent covers the Grant path inside the tx
// when a new role is added (pub != nil): EventTenantRoleGranted is emitted.
func TestReconcileRoles_GrantEmitsEvent(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()
	pub := &sg3RecPub{}
	txRunner := &sg3PubTxRunner{pub: pub}

	roles := &sg3FullRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil // currently holds no roles
		},
		grantFn: func(_ context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
			return tr, nil
		},
	}
	mem := &sg3MemRepo{}
	svc := buildReconcileRolesSvc(mem, roles, &port.TenantRepositoryNoop{}, txRunner, &fakeRPClient{})

	granted, revoked, err := svc.ReconcileRoles(context.Background(), tenantID, userID,
		[]domain.TenantRoleCode{domain.RoleTenderAdmin}, actorID)
	require.NoError(t, err)
	require.Len(t, granted, 1)
	assert.Empty(t, revoked)
	assert.Equal(t, domain.RoleTenderAdmin, granted[0].RoleCode)

	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTenantRoleGranted, pub.events[0].Type)
}

// TestReconcileRoles_RevokeEmitsEvent covers the Revoke path inside the tx
// when a previously-held role is removed (pub != nil): EventTenantRoleRevoked.
func TestReconcileRoles_RevokeEmitsEvent(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()
	pub := &sg3RecPub{}
	txRunner := &sg3PubTxRunner{pub: pub}

	roles := &sg3FullRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
		},
		revokeFn: func(_ context.Context, _ uuid.UUID, _ uuid.UUID, code domain.TenantRoleCode) (*domain.TenantRole, error) {
			return &domain.TenantRole{RoleCode: code}, nil
		},
	}
	mem := &sg3MemRepo{}
	// desired=[] → revoke the existing TenderAdmin.
	svc := buildReconcileRolesSvc(mem, roles, &port.TenantRepositoryNoop{}, txRunner, &fakeRPClient{})

	granted, revoked, err := svc.ReconcileRoles(context.Background(), tenantID, userID,
		[]domain.TenantRoleCode{}, actorID)
	require.NoError(t, err)
	assert.Empty(t, granted)
	require.Len(t, revoked, 1)
	assert.Equal(t, domain.RoleTenderAdmin, revoked[0].RoleCode)

	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTenantRoleRevoked, pub.events[0].Type)
}

// TestReconcileRoles_StripOwner_CountActiveOwnersInsideTxError covers
// lines 315-320: strippingOwner=true → tx takes lock → CountActiveOwners
// inside tx fails.
func TestReconcileRoles_StripOwner_CountActiveOwnersInsideTxError(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()
	caoErr := errors.New("count_owners_in_tx_error")

	// First ListByUser call (outside tx, pre-check): user holds tenant_owner.
	// CountActiveOwners (outside tx): 2 → fast-path doesn't reject.
	// Inside tx: CountActiveOwners → error.
	callCount := 0
	roles := &sg3FullRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) {
			callCount++
			if callCount == 1 {
				return 2, nil // outside tx: 2 owners → fast path passes
			}
			return 0, caoErr // inside tx: error
		},
	}
	tenants := &svcgapRuTenantRepo{lockErr: nil} // LockByID succeeds
	mem := &sg3MemRepo{}
	svc := buildReconcileRolesSvc(mem, roles, tenants, &passthroughTxRunner{}, nil)

	// desired does not include tenant_owner → strippingOwner=true.
	_, _, err := svc.ReconcileRoles(context.Background(), tenantID, userID,
		[]domain.TenantRoleCode{domain.RoleTenderAdmin}, actorID)
	assert.ErrorIs(t, err, caoErr)
}

// TestReconcileRoles_StripOwner_InsideTxLastOwner covers the last-owner
// guard inside the tx (lines 319-321): CountActiveOwners <= 1 → error.
func TestReconcileRoles_StripOwner_InsideTxLastOwner(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()

	callCount := 0
	roles := &sg3FullRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) {
			callCount++
			if callCount == 1 {
				return 2, nil // outside tx passes
			}
			return 1, nil // inside tx: last owner → reject
		},
	}
	tenants := &svcgapRuTenantRepo{lockErr: nil}
	mem := &sg3MemRepo{}
	svc := buildReconcileRolesSvc(mem, roles, tenants, &passthroughTxRunner{}, nil)

	_, _, err := svc.ReconcileRoles(context.Background(), tenantID, userID,
		[]domain.TenantRoleCode{domain.RoleTenderAdmin}, actorID)
	assert.ErrorIs(t, err, domain.ErrLastOwnerRemoval)
}

// TestReconcileRoles_GrantError covers the roles.Grant error inside the tx.
func TestReconcileRoles_GrantError(t *testing.T) {
	grantErr := errors.New("grant_failed")
	roles := &sg3FullRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
		grantFn: func(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
			return nil, grantErr
		},
	}
	mem := &sg3MemRepo{}
	svc := buildReconcileRolesSvc(mem, roles, &port.TenantRepositoryNoop{}, &passthroughTxRunner{}, nil)

	_, _, err := svc.ReconcileRoles(context.Background(), uuid.New(), uuid.New(),
		[]domain.TenantRoleCode{domain.RoleTenderAdmin}, uuid.New())
	assert.ErrorIs(t, err, grantErr)
}

// TestReconcileRoles_RevokeError covers the roles.Revoke error inside the tx.
func TestReconcileRoles_RevokeError(t *testing.T) {
	revokeErr := errors.New("revoke_failed")
	roles := &sg3FullRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
		},
		revokeFn: func(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ domain.TenantRoleCode) (*domain.TenantRole, error) {
			return nil, revokeErr
		},
	}
	mem := &sg3MemRepo{}
	svc := buildReconcileRolesSvc(mem, roles, &port.TenantRepositoryNoop{}, &passthroughTxRunner{}, nil)

	_, _, err := svc.ReconcileRoles(context.Background(), uuid.New(), uuid.New(),
		[]domain.TenantRoleCode{}, uuid.New())
	assert.ErrorIs(t, err, revokeErr)
}

// TestReconcileRoles_RPRevokeCalledWhenRolesRevoked covers the AUTH-8
// best-effort RevokeUserSessions call when len(revoked) > 0 (line 391).
func TestReconcileRoles_RPRevokeCalledWhenRolesRevoked(t *testing.T) {
	roles := &sg3FullRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
		},
	}
	mem := &sg3MemRepo{}
	rp := &fakeRPClient{}
	svc := buildReconcileRolesSvc(mem, roles, &port.TenantRepositoryNoop{}, &passthroughTxRunner{}, rp)

	// desired=[] → revoke TenderAdmin → rp.RevokeUserSessions called.
	_, _, err := svc.ReconcileRoles(context.Background(), uuid.New(), uuid.New(),
		[]domain.TenantRoleCode{}, uuid.New())
	require.NoError(t, err)
	assert.True(t, rp.revokeCalled, "AUTH-8: RevokeUserSessions must be called when roles are revoked")
}

// ═══════════════════════════════════════════════════════════════════════════
// membership_service.go — SetStatus owner-path tx (lines 184-186)
// ═══════════════════════════════════════════════════════════════════════════

// TestMembership_SetStatus_Suspend_OwnerLastOwner covers the LockByID path
// when suspending a tenant_owner (TM-13) who is the last owner → error.
func TestMembership_SetStatus_Suspend_OwnerLastOwner(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()

	m := &fakeMembershipRepo{
		setStatusFn: func(_ context.Context, _, _ uuid.UUID, s domain.MembershipStatus, _ int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: s}, nil
		},
	}
	// Return tenant_owner so the TM-13 code path is taken.
	ownerRoles := &sg3FullRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) {
			return 1, nil // last owner → error
		},
	}
	rp := &fakeRPClient{}
	// Use passthroughTxRunner so LockByID succeeds (TenantRepositoryNoop).
	svc := service.NewMembershipService(m, ownerRoles, nil, &port.TenantRepositoryNoop{}, nil, nil, rp, nil, &passthroughTxRunner{}, nil, 30)

	_, err := svc.SetStatus(context.Background(), tenantID, userID, domain.MembershipSuspended, 1)
	assert.ErrorIs(t, err, domain.ErrLastOwnerRemoval)
}

// TestMembership_SetStatus_Suspend_OwnerMoreOwners covers the success path
// where the user holds tenant_owner but is NOT the last one (lines 175-203).
func TestMembership_SetStatus_Suspend_OwnerMoreOwners(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()

	m := &fakeMembershipRepo{
		setStatusFn: func(_ context.Context, _, _ uuid.UUID, s domain.MembershipStatus, _ int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: s}, nil
		},
	}
	ownerRoles := &sg3FullRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) {
			return 2, nil // 2 owners → suspend allowed
		},
	}
	rp := &fakeRPClient{}
	svc := service.NewMembershipService(m, ownerRoles, nil, &port.TenantRepositoryNoop{}, nil, nil, rp, nil, &passthroughTxRunner{}, nil, 30)

	res, err := svc.SetStatus(context.Background(), tenantID, userID, domain.MembershipSuspended, 1)
	require.NoError(t, err)
	assert.Equal(t, domain.MembershipSuspended, res.Membership.Status)
	assert.True(t, rp.revokeCalled, "AUTH-8: RevokeUserSessions must be called on suspend")
}

// ═══════════════════════════════════════════════════════════════════════════
// membership_service.go — RemoveUser event emission (lines 496,514,536)
// ═══════════════════════════════════════════════════════════════════════════

// TestRemoveUser_EmitsAllEvents covers the pub != nil event emission blocks
// for TenantRoleRevoked (line 496), DepartmentMembershipRevoked (line 514),
// and MembershipRevoked (line 536) inside RemoveUser's tx.
func TestRemoveUser_EmitsAllEvents(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()
	pub := &sg3RecPub{}
	txRunner := &sg3PubTxRunner{pub: pub}

	mem := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	roles := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil // not an owner
		},
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
		},
	}
	deptMems := &ssDeptMemRepoDM{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return []domain.DeptMembership{{ID: uuid.New(), DepartmentID: uuid.New()}}, nil
		},
	}
	tenants := &svcgapRuTenantRepo{lockErr: nil}
	svc := service.NewMembershipService(mem, roles, deptMems, tenants, nil, nil, nil, nil, txRunner, nil, 30)

	err := svc.RemoveUser(context.Background(), tenantID, userID, actorID)
	require.NoError(t, err)

	types := make([]string, 0, len(pub.events))
	for _, e := range pub.events {
		types = append(types, e.Type)
	}
	assert.Contains(t, types, domain.EventTenantRoleRevoked)
	assert.Contains(t, types, domain.EventDepartmentMembershipRevoked)
	assert.Contains(t, types, domain.EventMembershipRevoked)
}

// ═══════════════════════════════════════════════════════════════════════════
// membership_service.go — ResetUserMFA requestctx envelope (line 711-714)
// ═══════════════════════════════════════════════════════════════════════════

// TestMembership_ResetUserMFA_RequestCtxEnvelope covers lines 711-714:
// when requestctx is in tx context, evt.IPAddress/UserAgent are set.
func TestMembership_ResetUserMFA_RequestCtxEnvelope(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()

	mem := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive}, nil
		},
	}
	rp := &fakeRPClient{}
	pub := &sg3RecPub{}

	// Inject both publisher and requestctx.
	txRunner := &svcgapDeptMemPubTxRunner{
		pub: pub,
		rc: &requestctx.RequestContext{
			TenantID:  tenantID,
			UserID:    actorID,
			ClientIP:  "1.2.3.4",
			UserAgent: "mfa-agent/1",
		},
	}

	svc := service.NewMembershipService(mem, noOwnerRoleRepo{}, nil, &port.TenantRepositoryNoop{}, nil, nil, rp, nil, txRunner, nil, 30)
	err := svc.ResetUserMFA(context.Background(), tenantID, userID, actorID)
	require.NoError(t, err)

	require.Len(t, pub.events, 1)
	assert.Equal(t, "1.2.3.4", pub.events[0].IPAddress)
	assert.Equal(t, "mfa-agent/1", pub.events[0].UserAgent)
}

// ═══════════════════════════════════════════════════════════════════════════
// membership_service.go — ValidateAndEmitAssigneeOverride requestctx (line 660)
// ═══════════════════════════════════════════════════════════════════════════

// TestValidateAndEmit_RequestCtxEnvelope covers lines 660-663:
// when requestctx is in tx context, evt.IPAddress/UserAgent are set.
func TestValidateAndEmit_RequestCtxEnvelope(t *testing.T) {
	tenantID, tenderID, newUserID, deptID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()

	actorRoles := []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}
	roleRepo := &fakeRoleRepo{
		listByUserFn: func(_ context.Context, _ uuid.UUID, uid uuid.UUID) ([]domain.TenantRole, error) {
			if uid == actorID {
				return actorRoles, nil
			}
			return nil, nil
		},
	}
	memRepo := &fakeMemRepoFull{
		findByUserFn: func(_ context.Context, _, _ uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive}, nil
		},
		listFn: func(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
			return &domain.MembershipListPage{}, nil
		},
		countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil },
	}
	pub := &sg3RecPub{}
	txRunner := &svcgapDeptMemPubTxRunner{
		pub: pub,
		rc: &requestctx.RequestContext{
			TenantID:  tenantID,
			UserID:    actorID,
			ClientIP:  "9.8.7.6",
			UserAgent: "override-agent/3",
		},
	}

	svc := service.NewMembershipService(memRepo, roleRepo,
		&svcgapAssigneeEligibleDM{deptID: deptID, level: domain.DeptApprover},
		&port.TenantRepositoryNoop{}, nil, nil, nil, nil, txRunner, nil, 30)

	err := svc.ValidateAndEmitAssigneeOverride(context.Background(),
		tenantID, tenderID, newUserID, deptID, domain.DeptApprover, actorID)
	require.NoError(t, err)

	require.Len(t, pub.events, 1)
	assert.Equal(t, "9.8.7.6", pub.events[0].IPAddress)
	assert.Equal(t, "override-agent/3", pub.events[0].UserAgent)
}
