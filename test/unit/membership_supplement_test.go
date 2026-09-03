// membership_supplement_test.go — targets uncovered branches in
// membership_service.go identified from the merged coverage report.
//
// Covered gaps:
//
//	List (90.0%):
//	  - deptMemberships.ListByUser error in the per-item hydration loop
//
//	SetStatus (88.5%):
//	  - roles.ListByUser error on the suspend path (pre-owner check)
//	  - owner-path tx-unavailable branch (pgadapterTxFromContext !ok)
//
//	ReconcileRoles (87.3%):
//	  - roles.ListByUser error after FindByUserID
//	  - roles.CountActiveOwners error inside tx when strippingOwner=true
//	  - roles.Grant fails inside tx
//	  - roles.Revoke fails inside tx
//	  - cache != nil branch for per-user cacheKeyMemberships delete
//
//	RemoveUser (88.3%):
//	  - roles.ListByUser error inside tx (after FindByUserID)
//	  - deptMemberships.SoftDeleteAllForUser error
//	  - memberships.SoftDelete error
//
//	RemovalResolution (93.8%):
//	  - *replacementUserID == userID self-replacement guard
//
//	ValidateAndEmitAssigneeOverride (83.9%):
//	  - pub == nil path (txRunner context carries no publisher — returns nil)
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

// ── helpers ────────────────────────────────────────────────────────────────

// noErrRoleRepo is a minimal TenantRoleRepository whose ListByUser always
// returns the provided slice. Used where we want to bypass the suspend/owner
// check without triggering a nil deref.
type noErrRoleRepo struct {
	roles []domain.TenantRole
	err   error
}

func (r *noErrRoleRepo) ListByUser(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
	return r.roles, r.err
}
func (r *noErrRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *noErrRoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) {
	return 2, nil
}
func (r *noErrRoleRepo) Grant(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
	return nil, nil
}
func (r *noErrRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (r *noErrRoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*noErrRoleRepo)(nil)

// ═══════════════════════════════════════════════════════════════════════════
// List (P-4) — deptMemberships.ListByUser error in the per-item hydration
// ═══════════════════════════════════════════════════════════════════════════

// Test Case ID:      LIST-DEPT-ERR-01
// Feature:           P-4 List · deptMemberships.ListByUser error during
//
//	per-item hydration → error surfaces, no partial result
//
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestMembership_List_DeptFetchErrorInLoopAborts(t *testing.T) {
	page := &domain.MembershipListPage{
		Items: []domain.MembershipListItem{
			{Membership: domain.TenantMembership{UserID: uuid.New()}},
		},
	}
	m := &fakeMemRepoFull{
		listFn: func(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
			return page, nil
		},
	}
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
	}
	deptErr := errors.New("dept listing failed during hydration")
	dm := &fakeDeptMemListByUser{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return nil, deptErr
		},
	}
	svc := buildMembershipSvc(m, r, dm, nil, nil, nil)
	_, err := svc.List(context.Background(), uuid.New(), nil, 50)
	assert.ErrorIs(t, err, deptErr, "deptMemberships.ListByUser error must propagate from the per-item hydration loop")
}

// ═══════════════════════════════════════════════════════════════════════════
// SetStatus (P-7) — uncovered branches
// ═══════════════════════════════════════════════════════════════════════════

// Test Case ID:      P7-ROLES-LIST-ERR-01
// Feature:           P-7 suspend · roles.ListByUser error before TM-13 check
//
//	→ error surfaces, no state change
//
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestMembership_SetStatus_Suspend_RolesListError_ShortCircuits(t *testing.T) {
	// On the suspend path the service first calls roles.ListByUser to determine
	// whether the target holds tenant_owner (TM-13 fast-path). If that call
	// errors the operation must abort before touching any state.
	rolesErr := errors.New("roles db down")
	roles := &noErrRoleRepo{err: rolesErr}
	rp := &fakeRPClient{}
	svc := service.NewMembershipService(
		&fakeMembershipRepo{}, roles, nil, nil, nil,
		nil, rp, nil, nil, nil, 30,
	)
	_, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	assert.ErrorIs(t, err, rolesErr, "roles.ListByUser error must propagate before any state change")
	assert.False(t, rp.revokeCalled, "RP must not be called when roles check fails")
}

// Test Case ID:      P7-OWNER-TX-UNAVAILABLE-01
// Feature:           P-7 suspend owner · pgadapterTxFromContext !ok → ErrConflict
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestMembership_SetStatus_Suspend_OwnerTxUnavailable_ReturnsConflict(t *testing.T) {
	// When the target is a tenant_owner the suspend path enters RunInTx. If the
	// txRunner does NOT inject a pgx.Tx into the context (e.g. a unit-test
	// passthroughTxRunner), pgadapterTxFromContext returns !ok and the service
	// must return ErrConflict rather than panicking or silently succeeding.
	roles := &ownerRoleRepo{countFn: func(context.Context, uuid.UUID) (int, error) { return 2, nil }}
	m := &fakeMembershipRepo{
		setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipSuspended}, nil
		},
	}
	// passthroughTxRunner does NOT inject a pgx.Tx → pgadapterTxFromContext !ok.
	svc := service.NewMembershipService(
		m, roles, nil, nil, nil,
		nil, &fakeRPClient{}, nil, &passthroughTxRunner{}, nil, 30,
	)
	_, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	assert.ErrorIs(t, err, domain.ErrConflict, "owner-path suspend with no tx must return ErrConflict")
}

// ═══════════════════════════════════════════════════════════════════════════
// ReconcileRoles (P-28) — uncovered branches
// ═══════════════════════════════════════════════════════════════════════════

// Test Case ID:      P28-ROLES-LIST-ERR-01
// Feature:           P-28 · roles.ListByUser error after FindByUserID → propagates
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestReconcileRoles_RolesListError_Propagates(t *testing.T) {
	// FindByUserID succeeds; the subsequent roles.ListByUser call to hydrate
	// currentSet fails. The error must propagate immediately.
	rolesErr := errors.New("roles db unavailable")
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New()}, nil
		},
	}
	roles := &noErrRoleRepo{err: rolesErr}
	svc := buildMembershipSvcWithTx(m, roles)
	_, _, err := svc.ReconcileRoles(context.Background(), uuid.New(), uuid.New(),
		[]domain.TenantRoleCode{domain.RoleTenantAdmin}, uuid.New())
	assert.ErrorIs(t, err, rolesErr)
}

// TestReconcileRoles_StrippingOwner_PreTxCountError_Propagates covers line
// 297.18,299.5: CountActiveOwners error on the pre-tx fast-path check when
// strippingOwner=true. This is distinct from the in-tx re-check error.
func TestReconcileRoles_StrippingOwner_PreTxCountError_Propagates(t *testing.T) {
	countErr := errors.New("pre-tx count error")
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID}

	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) {
			// Pre-tx check immediately fails → must propagate.
			return 0, countErr
		},
	}
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	svc := buildMembershipSvcWithTx(m, roles)
	// desired=[] → removing the owner role → strippingOwner=true → pre-tx CountActiveOwners called
	_, _, err := svc.ReconcileRoles(context.Background(), tenantID, userID,
		[]domain.TenantRoleCode{}, uuid.New())
	assert.ErrorIs(t, err, countErr,
		"CountActiveOwners error on pre-tx fast-path must propagate immediately")
}

// Test Case ID:      P28-COUNT-OWNERS-TX-ERR-01
// Feature:           P-28 · strippingOwner=true, CountActiveOwners fails inside tx → propagates
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestReconcileRoles_StrippingOwner_CountOwnersTxError_Propagates(t *testing.T) {
	// Owner is being stripped (currentSet has owner, desired does not). The pre-tx
	// check sees ownerCount=2 (allowed). Inside the tx the authoritative recheck
	// (CountActiveOwners) fails — this error must propagate.
	countCallNum := 0
	countErr := errors.New("count owners tx failed")
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID}

	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) {
			countCallNum++
			if countCallNum == 1 {
				// Pre-tx check: 2 owners → allowed (no fast-path rejection).
				return 2, nil
			}
			// Inside-tx authoritative recheck: error.
			return 0, countErr
		},
	}
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	svc := buildMembershipSvcWithTx(m, roles)
	// desired=[] → removing the owner role → strippingOwner=true
	_, _, err := svc.ReconcileRoles(context.Background(), tenantID, userID,
		[]domain.TenantRoleCode{}, uuid.New())
	assert.ErrorIs(t, err, countErr,
		"CountActiveOwners error inside tx must propagate (authoritative TM-8 re-check)")
}

// Test Case ID:      P28-GRANT-TX-ERR-01
// Feature:           P-28 · roles.Grant fails inside tx → propagates
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestReconcileRoles_GrantInsideTx_ErrorPropagates(t *testing.T) {
	grantErr := errors.New("grant constraint violation")
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID}

	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil // plain member — currentSet is empty
		},
		grantFn: func(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
			return nil, grantErr
		},
	}
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	svc := buildMembershipSvcWithTx(m, roles)
	// desired=[tenant_admin] → Grant is called; it fails.
	_, _, err := svc.ReconcileRoles(context.Background(), tenantID, userID,
		[]domain.TenantRoleCode{domain.RoleTenantAdmin}, uuid.New())
	assert.ErrorIs(t, err, grantErr)
}

// Test Case ID:      P28-REVOKE-TX-ERR-01
// Feature:           P-28 · roles.Revoke fails inside tx → propagates
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestReconcileRoles_RevokeInsideTx_ErrorPropagates(t *testing.T) {
	revokeErr := errors.New("revoke db timeout")
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID}

	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			// User currently holds tender_admin.
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
		},
		revokeFn: func(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
			return nil, revokeErr
		},
	}
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	svc := buildMembershipSvcWithTx(m, roles)
	// desired=[] → tender_admin must be revoked; Revoke fails.
	_, _, err := svc.ReconcileRoles(context.Background(), tenantID, userID,
		[]domain.TenantRoleCode{}, uuid.New())
	assert.ErrorIs(t, err, revokeErr)
}

// Test Case ID:      P28-CACHE-MEMBERSHIPS-01
// Feature:           P-28 · cache != nil → per-user cacheKeyMemberships deleted on success
// Priority: P2 · Severity: Minor · Automation Status: Automated
func TestReconcileRoles_CacheNotNil_InvalidatesPerUserMembershipsKey(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID}

	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil // plain member
		},
		grantFn: func(_ context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
			return tr, nil
		},
	}
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return mem, nil
		},
	}
	cache := &spyCache{}
	svc := service.NewMembershipService(
		m, roles, nil, nil, nil,
		cache, &fakeRPClient{}, nil, &passthroughTxRunner{}, nil, 0,
	)
	_, _, err := svc.ReconcileRoles(context.Background(), tenantID, userID,
		[]domain.TenantRoleCode{domain.RoleTenantAdmin}, uuid.New())
	require.NoError(t, err)
	// The per-user I-8 cache key must be invalidated alongside the members list key.
	found := false
	for _, k := range cache.deleteCalls {
		if k == "om:memberships:"+tenantID.String()+":"+userID.String() {
			found = true
			break
		}
	}
	assert.True(t, found, "cacheKeyMemberships(tenantID, userID) must be deleted after ReconcileRoles succeeds")
}

// ═══════════════════════════════════════════════════════════════════════════
// RemoveUser (P-8) — uncovered branches
// ═══════════════════════════════════════════════════════════════════════════

// Test Case ID:      P8-ROLES-LIST-TX-ERR-01
// Feature:           P-8 · roles.ListByUser error inside tx (after FindByUserID)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestMembership_RemoveUser_RolesListInsideTxError_Propagates(t *testing.T) {
	rolesErr := errors.New("roles list inside tx failed")
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	r := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, rolesErr
		},
		// countActiveOwnersFn is irrelevant — we never reach it.
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) { return 2, nil },
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	svc := buildRemoveUserSvc(m, r, nil, nil, wf, &ruTxRunner{})
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, rolesErr)
}

// Test Case ID:      P8-DEPT-CASCADE-ERR-01
// Feature:           P-8 · deptMemberships.SoftDeleteAllForUser error → propagates
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestMembership_RemoveUser_DeptCascadeError_Propagates(t *testing.T) {
	deptErr := errors.New("dept cascade failed")
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	r := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil // plain member, no wasOwner branch
		},
		countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 2, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	dm := &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return nil, deptErr
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	svc := buildRemoveUserSvc(m, r, dm, nil, wf, &ruTxRunner{})
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, deptErr)
}

// Test Case ID:      P8-SOFT-DELETE-MEM-ERR-01
// Feature:           P-8 · memberships.SoftDelete error (step 5) → propagates
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestMembership_RemoveUser_SoftDeleteMembershipError_Propagates(t *testing.T) {
	softDelErr := errors.New("soft delete membership failed")
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error {
			return softDelErr
		},
	}
	r := &ruRoleRepo{
		listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 2, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	dm := &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return nil, nil
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	pub := &ruPublisher{}
	tr := &ruTxRunner{pub: pub}
	svc := buildRemoveUserSvc(m, r, dm, nil, wf, tr)
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, softDelErr)
}

// ═══════════════════════════════════════════════════════════════════════════
// RemovalResolution (P-26) — self-replacement guard
// ═══════════════════════════════════════════════════════════════════════════

// Test Case ID:      P26-SELF-REPLACE-01
// Feature:           P-26 replace_delegate · replacementUserID == userID → ErrInvalidReplacement
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestMembership_RemovalResolution_ReplaceDelegate_SelfReplacementRejected(t *testing.T) {
	// WFI-5 guard: the replacement cannot be the same user being removed.
	// This branch (*replacementUserID == userID) precedes the FindByUserID call,
	// so no membership repo is needed — the check is purely in-memory.
	userID := uuid.New()
	wf := &spyWorkflowClient{}
	svc := buildMembershipSvcForRemoval(nil, nil, nil, wf, nil)

	err := svc.RemovalResolution(context.Background(), uuid.New(), userID,
		service.RemovalReplaceDelegate, &userID, uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrInvalidReplacement, de.Cause,
		"self-replacement must be rejected with ErrInvalidReplacement")
	assert.Equal(t, "invalid_replacement", de.Details["code"])
	assert.False(t, wf.reassignCalled, "workflow must not be contacted for a self-replacement rejection")
}

// ═══════════════════════════════════════════════════════════════════════════
// ValidateAndEmitAssigneeOverride (I-13) — pub != nil path (event emission)
// ═══════════════════════════════════════════════════════════════════════════

// TestMembership_ValidateAndEmitAssigneeOverride_WithPub_EmitsEvent covers
// lines 667-679 in membership_service.go: the event construction block inside
// ValidateAndEmitAssigneeOverride's RunInTx closure when pub != nil.
func TestMembership_ValidateAndEmitAssigneeOverride_WithPub_EmitsEvent(t *testing.T) {
	deptID := uuid.New()
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
		},
	}
	m := &fakeMemRepoFull{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive}, nil
		},
	}
	dm := &fakeDeptMemListByUser{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return []domain.DeptMembership{{DepartmentID: deptID, RoleLevel: domain.DeptApprover}}, nil
		},
	}

	// Use a tx runner that injects a real ContextEventPublisher.
	evtCapture := &assigneeOverridePub{}
	txRunner := assigneeTxRunner{pub: evtCapture}

	svc := buildMembershipSvcForRemoval(m, r, dm, nil, txRunner)

	err := svc.ValidateAndEmitAssigneeOverride(context.Background(),
		uuid.New(), uuid.New(), uuid.New(), deptID, domain.DeptReviewer, uuid.New())
	require.NoError(t, err, "ValidateAndEmitAssigneeOverride with publisher must succeed")
	require.Len(t, evtCapture.events, 1, "TenderAssigneeOverridden event must be emitted")
	assert.Equal(t, domain.EventTenderAssigneeOverridden, evtCapture.events[0].Type)
}

// assigneeOverridePub captures events via Enqueue.
type assigneeOverridePub struct {
	events []*domain.DomainEvent
}

func (p *assigneeOverridePub) Enqueue(_ context.Context, e *domain.DomainEvent) error {
	p.events = append(p.events, e)
	return nil
}

var _ port.EventPublisher = (*assigneeOverridePub)(nil)

// assigneeTxRunner injects an event publisher into the tx context.
type assigneeTxRunner struct {
	pub port.EventPublisher
}

func (r assigneeTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	ctx = port.WithEventPublisher(ctx, r.pub)
	return fn(ctx)
}

var _ port.TxRunner = assigneeTxRunner{}

// ValidateAndEmitAssigneeOverride (I-13) — pub == nil path
// ═══════════════════════════════════════════════════════════════════════════

// Test Case ID:      I13-PUB-NIL-01
// Feature:           I-13 · txRunner carries no event publisher → returns nil (no panic)
//
//	The passthroughTxRunner does not inject an event publisher, so
//	port.EventPublisherFromContext returns (nil, false). The service
//	must detect pub==nil and return nil rather than panicking.
//
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestMembership_ValidateAndEmitAssigneeOverride_PubNil_ReturnsNil(t *testing.T) {
	deptID := uuid.New()
	r := &fakeRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
		},
	}
	m := &fakeMemRepoFull{
		findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{Status: domain.MembershipActive}, nil
		},
	}
	dm := &fakeDeptMemListByUser{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return []domain.DeptMembership{{DepartmentID: deptID, RoleLevel: domain.DeptApprover}}, nil
		},
	}
	// passthroughTxRunner does NOT inject an event publisher into the context.
	svc := buildMembershipSvcForRemoval(m, r, dm, nil, &passthroughTxRunner{})

	err := svc.ValidateAndEmitAssigneeOverride(context.Background(),
		uuid.New(), uuid.New(), uuid.New(), deptID, domain.DeptReviewer, uuid.New())
	require.NoError(t, err,
		"pub==nil (no publisher in txCtx) must return nil — service must not panic")
}
