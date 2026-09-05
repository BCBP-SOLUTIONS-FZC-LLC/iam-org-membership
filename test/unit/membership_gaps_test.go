// Additional unit tests closing remaining branch gaps in
// internal/core/service/membership_service.go: List (dept-view population +
// dept-fetch error), SetStatus (owner-suspend tx error branches),
// ReconcileRoles (validation/outer-check/inner-race/event/cache branches),
// RemoveUser (role-list/owner-count/requestctx/event-enqueue-error/
// soft-delete-error branches), and ValidateAndEmitAssigneeOverride (event
// emission with a live publisher + requestctx).
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

// ── List — dept fetch error aborts + dept-view population ──────────────

func TestMembership_List_DeptFetchErrorAborts(t *testing.T) {
	page := &domain.MembershipListPage{
		Items: []domain.MembershipListItem{{Membership: domain.TenantMembership{UserID: uuid.New()}}},
	}
	deptErr := errors.New("dept lookup failed")
	m := &fakeMemRepoFull{
		listFn: func(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
			return page, nil
		},
	}
	r := &fakeRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil }}
	dm := &fakeDeptMemListByUser{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
		return nil, deptErr
	}}
	_, err := buildMembershipSvc(m, r, dm, nil, nil, nil).List(context.Background(), uuid.New(), nil, 50)
	assert.ErrorIs(t, err, deptErr)
}

func TestMembership_List_PopulatesDepartmentViews(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	page := &domain.MembershipListPage{
		Items: []domain.MembershipListItem{{Membership: domain.TenantMembership{TenantID: tenantID, UserID: userID}}},
	}
	m := &fakeMemRepoFull{
		listFn: func(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
			return page, nil
		},
	}
	r := &fakeRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil }}
	dm := &fakeDeptMemListByUser{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
		return []domain.DeptMembership{{DepartmentID: deptID, RoleLevel: domain.DeptApprover}}, nil
	}}
	got, err := buildMembershipSvc(m, r, dm, nil, nil, nil).List(context.Background(), tenantID, nil, 50)
	require.NoError(t, err)
	require.Len(t, got.Items, 1)
	require.Len(t, got.Items[0].Departments, 1)
	assert.Equal(t, deptID, got.Items[0].Departments[0].DepartmentID)
	assert.Equal(t, domain.DeptApprover, got.Items[0].Departments[0].RoleLevel)
}

// ── SetStatus — owner-suspend path: outer roles error + tx-level errors ─

func TestMembership_SetStatus_SuspendOuterRolesListErrorPropagates(t *testing.T) {
	rolesErr := errors.New("roles unavailable")
	r := &fakeRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
		return nil, rolesErr
	}}
	svc := service.NewMembershipService(&fakeMembershipRepo{}, r, nil, &port.TenantRepositoryNoop{}, nil, nil, &fakeRPClient{}, nil, nil, nil, 30)
	_, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	assert.ErrorIs(t, err, rolesErr)
}

func TestMembership_SetStatus_OwnerSuspend_TenantLockErrorPropagates(t *testing.T) {
	lockErr := errors.New("lock timeout")
	roles := &ownerRoleRepo{countFn: func(context.Context, uuid.UUID) (int, error) { return 5, nil }}
	tenants := &ruTenantRepo{lockErr: lockErr}
	svc := service.NewMembershipService(&fakeMembershipRepo{}, roles, nil, tenants, nil, nil, &fakeRPClient{}, nil, &ruTxRunner{}, nil, 30)

	_, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	assert.ErrorIs(t, err, lockErr)
}

func TestMembership_SetStatus_OwnerSuspend_InnerCountActiveOwnersErrorPropagates(t *testing.T) {
	countErr := errors.New("count query failed")
	roles := &ownerRoleRepo{countFn: func(context.Context, uuid.UUID) (int, error) {
		return 0, countErr
	}}
	svc := service.NewMembershipService(&fakeMembershipRepo{}, roles, nil, &port.TenantRepositoryNoop{}, nil, nil, &fakeRPClient{}, nil, &ruTxRunner{}, nil, 30)

	_, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	assert.ErrorIs(t, err, countErr)
}

// ── ReconcileRoles — validation: RoleMember in desired set ──────────────

func TestReconcileRoles_DesiredIncludesMember_ValidationError(t *testing.T) {
	svc := buildMembershipSvc(&fakeMemRepoFull{}, nil, nil, nil, nil, nil)
	_, _, err := svc.ReconcileRoles(context.Background(), uuid.New(), uuid.New(),
		[]domain.TenantRoleCode{domain.RoleMember}, uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "invalid_role", de.Details["code"])
	assert.Contains(t, de.Error(), "member cannot be granted")
}

// ── ReconcileRoles — outer roles.ListByUser error ───────────────────────

func TestReconcileRoles_OuterRolesListByUserErrorPropagates(t *testing.T) {
	listErr := errors.New("roles unavailable")
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New()}, nil
		},
	}
	roles := &extRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
		return nil, listErr
	}}
	svc := buildMembershipSvcWithTx(m, roles)
	_, _, err := svc.ReconcileRoles(context.Background(), uuid.New(), uuid.New(),
		[]domain.TenantRoleCode{domain.RoleTenantAdmin}, uuid.New())
	assert.ErrorIs(t, err, listErr)
}

// ── ReconcileRoles — outer TM-8 CountActiveOwners error ─────────────────

func TestReconcileRoles_OuterCountActiveOwnersErrorPropagates(t *testing.T) {
	countErr := errors.New("count query failed")
	m := &fakeMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New()}, nil
		},
	}
	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) { return 0, countErr },
	}
	svc := buildMembershipSvcWithTx(m, roles)
	// Stripping the owner role (desired=[]) triggers the outer TM-8 count check.
	_, _, err := svc.ReconcileRoles(context.Background(), uuid.New(), uuid.New(),
		[]domain.TenantRoleCode{}, uuid.New())
	assert.ErrorIs(t, err, countErr)
}

// ── ReconcileRoles — TM-13 lock error inside tx ─────────────────────────

func TestReconcileRoles_TenantLockErrorInsideTxPropagates(t *testing.T) {
	lockErr := errors.New("lock timeout")
	mem := &domain.TenantMembership{ID: uuid.New()}
	m := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return mem, nil
	}}
	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) { return 2, nil }, // passes the outer check
	}
	tenants := &ruTenantRepo{lockErr: lockErr}
	svc := service.NewMembershipService(m, roles, nil, tenants, nil, nil, &fakeRPClient{}, nil, &passthroughTxRunner{}, nil, 0)

	_, _, err := svc.ReconcileRoles(context.Background(), uuid.New(), uuid.New(), []domain.TenantRoleCode{}, uuid.New())
	assert.ErrorIs(t, err, lockErr)
}

// ── ReconcileRoles — race: outer check passes (owners=2), inner recheck
//    inside the tx sees owners=1 (a concurrent removal already landed) ──

type raceOwnerRoleRepo struct {
	*extRoleRepo
	calls int
}

func (r *raceOwnerRoleRepo) CountActiveOwners(ctx context.Context, tenantID uuid.UUID) (int, error) {
	r.calls++
	if r.calls == 1 {
		return 2, nil // outer pre-check: passes
	}
	return 1, nil // inner authoritative recheck: a race dropped it to 1
}

func TestReconcileRoles_InnerRecheckRace_BlocksLastOwner(t *testing.T) {
	mem := &domain.TenantMembership{ID: uuid.New()}
	m := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return mem, nil
	}}
	base := &extRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
		return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
	}}
	roles := &raceOwnerRoleRepo{extRoleRepo: base}
	svc := buildMembershipSvcWithTx(m, roles)

	_, _, err := svc.ReconcileRoles(context.Background(), uuid.New(), uuid.New(), []domain.TenantRoleCode{}, uuid.New())
	assert.ErrorIs(t, err, domain.ErrLastOwnerRemoval)
	assert.Equal(t, 2, roles.calls, "outer pre-check + inner authoritative recheck must both run")
}

// ── ReconcileRoles — inner (in-tx) TM-8 CountActiveOwners error ────────

func TestReconcileRoles_InnerCountActiveOwnersErrorPropagates(t *testing.T) {
	countErr := errors.New("count query failed")
	mem := &domain.TenantMembership{ID: uuid.New()}
	m := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return mem, nil
	}}
	calls := 0
	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) {
			calls++
			if calls == 1 {
				return 2, nil // outer pre-check: passes
			}
			return 0, countErr // inner authoritative recheck: errors
		},
	}
	svc := buildMembershipSvcWithTx(m, roles)
	_, _, err := svc.ReconcileRoles(context.Background(), uuid.New(), uuid.New(), []domain.TenantRoleCode{}, uuid.New())
	assert.ErrorIs(t, err, countErr)
	assert.Equal(t, 2, calls, "outer pre-check + inner authoritative recheck must both run")
}

// ── ReconcileRoles — re-granting an already-held role is a no-op ───────

func TestReconcileRoles_AlreadyHeldRole_SkipsGrant(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID}
	grantCalled := false
	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
		grantFn: func(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
			grantCalled = true
			return nil, nil
		},
	}
	m := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return mem, nil
	}}
	svc := buildMembershipSvcWithTx(m, roles)
	granted, _, err := svc.ReconcileRoles(context.Background(), tenantID, userID,
		[]domain.TenantRoleCode{domain.RoleTenantAdmin}, uuid.New())
	require.NoError(t, err)
	assert.Empty(t, granted, "a role already held must not be re-granted")
	assert.False(t, grantCalled)
}

// ── ReconcileRoles — Grant/Revoke repo errors propagate ─────────────────

func TestReconcileRoles_GrantErrorPropagates(t *testing.T) {
	grantErr := errors.New("grant failed")
	mem := &domain.TenantMembership{ID: uuid.New()}
	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		grantFn: func(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
			return nil, grantErr
		},
	}
	m := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return mem, nil
	}}
	svc := buildMembershipSvcWithTx(m, roles)
	_, _, err := svc.ReconcileRoles(context.Background(), uuid.New(), uuid.New(),
		[]domain.TenantRoleCode{domain.RoleTenantAdmin}, uuid.New())
	assert.ErrorIs(t, err, grantErr)
}

func TestReconcileRoles_RevokeErrorPropagates(t *testing.T) {
	revokeErr := errors.New("revoke failed")
	mem := &domain.TenantMembership{ID: uuid.New()}
	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
		revokeFn: func(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
			return nil, revokeErr
		},
	}
	m := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return mem, nil
	}}
	svc := buildMembershipSvcWithTx(m, roles)
	_, _, err := svc.ReconcileRoles(context.Background(), uuid.New(), uuid.New(),
		[]domain.TenantRoleCode{}, uuid.New())
	assert.ErrorIs(t, err, revokeErr)
}

// ── ReconcileRoles — event emission (Grant + Revoke) with requestctx ───

type rrPub struct{ events []*domain.DomainEvent }

func (p *rrPub) Enqueue(_ context.Context, e *domain.DomainEvent) error {
	p.events = append(p.events, e)
	return nil
}

type rrTxRunner struct{ pub port.EventPublisher }

func (r *rrTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	if r.pub != nil {
		ctx = port.WithEventPublisher(ctx, r.pub)
	}
	return fn(ctx)
}

func TestReconcileRoles_EmitsGrantAndRevokeEventsWithRequestContext(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID}
	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil // to be revoked
		},
	}
	m := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return mem, nil
	}}
	pub := &rrPub{}
	svc := service.NewMembershipService(m, roles, nil, &port.TenantRepositoryNoop{}, nil, nil, &fakeRPClient{}, nil, &rrTxRunner{pub: pub}, nil, 0)

	ctx := requestctx.WithContext(context.Background(), &requestctx.RequestContext{
		ClientIP: "192.0.2.55", UserAgent: "reconcile-test-agent",
	})
	// Grant tenant_admin (new) while implicitly revoking tender_admin (not in desired).
	granted, revoked, err := svc.ReconcileRoles(ctx, tenantID, userID,
		[]domain.TenantRoleCode{domain.RoleTenantAdmin}, actorID)
	require.NoError(t, err)
	require.Len(t, granted, 1)
	require.Len(t, revoked, 1)
	require.Len(t, pub.events, 2)

	for _, evt := range pub.events {
		assert.Equal(t, "192.0.2.55", evt.IPAddress)
		assert.Equal(t, "reconcile-test-agent", evt.UserAgent)
	}
	assert.Equal(t, domain.EventTenantRoleGranted, pub.events[0].Type)
	assert.Equal(t, domain.EventTenantRoleRevoked, pub.events[1].Type)
}

// ── ReconcileRoles — cache invalidation + AUTH-8 session revoke ────────

func TestReconcileRoles_InvalidatesCacheAndRevokesSessionsOnRevoke(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	mem := &domain.TenantMembership{ID: uuid.New(), TenantID: tenantID, UserID: userID}
	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
	}
	m := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return mem, nil
	}}
	cache := &spyCache{}
	rp := &fakeRPClient{}
	svc := service.NewMembershipService(m, roles, nil, &port.TenantRepositoryNoop{}, nil, cache, rp, nil, &passthroughTxRunner{}, nil, 0)

	_, revoked, err := svc.ReconcileRoles(context.Background(), tenantID, userID, []domain.TenantRoleCode{}, uuid.New())
	require.NoError(t, err)
	require.NotEmpty(t, revoked)
	assert.Contains(t, cache.deleteCalls, "om:memberships:"+tenantID.String()+":"+userID.String())
	assert.True(t, rp.revokeCalled, "AUTH-8: revoking a role must trigger RP session revoke")
}

// ── RemoveUser — roles.ListByUser error, owner-count error, requestctx ─

func TestMembership_RemoveUser_RolesListByUserErrorPropagates(t *testing.T) {
	listErr := errors.New("roles unavailable")
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	r := &ruRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
		return nil, listErr
	}}
	wf := &fakeWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{}, nil
	}}
	svc := buildRemoveUserSvc(m, r, nil, nil, wf, &ruTxRunner{})
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, listErr)
}

func TestMembership_RemoveUser_OwnerCountErrorPropagates(t *testing.T) {
	countErr := errors.New("count query failed")
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	r := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) { return 0, countErr },
	}
	wf := &fakeWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{}, nil
	}}
	svc := buildRemoveUserSvc(m, r, nil, nil, wf, &ruTxRunner{})
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, countErr)
}

func TestMembership_RemoveUser_RequestContextIPAndUAPropagateToEvents(t *testing.T) {
	tenantID, userID, actorID := uuid.New(), uuid.New(), uuid.New()
	s := setupHappyRemoveUser(t, tenantID, userID)

	ctx := requestctx.WithContext(context.Background(), &requestctx.RequestContext{
		ClientIP: "203.0.113.42", UserAgent: "removeuser-test-agent",
	})
	err := s.svc.RemoveUser(ctx, tenantID, userID, actorID)
	require.NoError(t, err)
	require.NotEmpty(t, s.pub.events)
	for _, evt := range s.pub.events {
		assert.Equal(t, "203.0.113.42", evt.IPAddress)
		assert.Equal(t, "removeuser-test-agent", evt.UserAgent)
	}
}

// ── RemoveUser — dept cascade error propagates ──────────────────────────

func TestMembership_RemoveUser_DeptCascadeErrorPropagates(t *testing.T) {
	deptErr := errors.New("dept cascade blew up")
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	r := &ruRoleRepo{
		listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 5, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	dm := &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
		return nil, deptErr
	}}
	wf := &fakeWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{}, nil
	}}
	svc := buildRemoveUserSvc(m, r, dm, nil, wf, &ruTxRunner{})
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, deptErr)
}

// ── RemoveUser — membership SoftDelete error propagates ────────────────

func TestMembership_RemoveUser_MembershipSoftDeleteErrorPropagates(t *testing.T) {
	softDeleteErr := errors.New("optimistic_lock_conflict")
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return softDeleteErr },
	}
	r := &ruRoleRepo{
		listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 5, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	dm := &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil }}
	wf := &fakeWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
		return &port.DelegateImpact{}, nil
	}}
	svc := buildRemoveUserSvc(m, r, dm, nil, wf, &ruTxRunner{})
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, softDeleteErr)
}

// ── RemoveUser — publisher Enqueue errors propagate for every event kind ─

type errAtPublisher struct {
	failAtIndex int
	calls       int
}

func (p *errAtPublisher) Enqueue(_ context.Context, _ *domain.DomainEvent) error {
	idx := p.calls
	p.calls++
	if idx == p.failAtIndex {
		return errors.New("enqueue failed")
	}
	return nil
}

func TestMembership_RemoveUser_RoleRevokedEventEnqueueErrorPropagates(t *testing.T) {
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	r := &ruRoleRepo{
		listByUserFn:        func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) { return 5, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
	}
	pub := &errAtPublisher{failAtIndex: 0} // first Enqueue call == the role-revoked event
	svc := buildRemoveUserSvc(m, r, nil, nil, nil, &ruTxRunner{pub: pub})
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorContains(t, err, "enqueue failed")
}

func TestMembership_RemoveUser_DeptRevokedEventEnqueueErrorPropagates(t *testing.T) {
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	r := &ruRoleRepo{
		listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 5, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	dm := &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
		return []domain.DeptMembership{{DepartmentID: uuid.New()}}, nil
	}}
	pub := &errAtPublisher{failAtIndex: 0} // no role events emitted → first call is the dept event
	svc := buildRemoveUserSvc(m, r, dm, nil, nil, &ruTxRunner{pub: pub})
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorContains(t, err, "enqueue failed")
}

func TestMembership_RemoveUser_MembershipRevokedEventEnqueueErrorPropagates(t *testing.T) {
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	r := &ruRoleRepo{
		listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 5, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	dm := &ruDeptMemRepo{softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil }}
	pub := &errAtPublisher{failAtIndex: 0} // no role/dept events → first (only) call is MembershipRevoked
	svc := buildRemoveUserSvc(m, r, dm, nil, nil, &ruTxRunner{pub: pub})
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorContains(t, err, "enqueue failed")
}

// ── ValidateAndEmitAssigneeOverride — event emission with live publisher ─

type vaePub struct{ events []*domain.DomainEvent }

func (p *vaePub) Enqueue(_ context.Context, e *domain.DomainEvent) error {
	p.events = append(p.events, e)
	return nil
}

type vaeTxRunner struct{ pub port.EventPublisher }

func (r *vaeTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	if r.pub != nil {
		ctx = port.WithEventPublisher(ctx, r.pub)
	}
	return fn(ctx)
}

func TestMembership_ValidateAndEmitAssigneeOverride_EmitsEventWithRequestContext(t *testing.T) {
	deptID, tenderID, newUserID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
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
	pub := &vaePub{}
	svc := buildMembershipSvcForRemoval(m, r, dm, nil, &vaeTxRunner{pub: pub})

	ctx := requestctx.WithContext(context.Background(), &requestctx.RequestContext{
		ClientIP: "198.51.100.23", UserAgent: "override-test-agent",
	})
	err := svc.ValidateAndEmitAssigneeOverride(ctx, uuid.New(), tenderID, newUserID, deptID, domain.DeptReviewer, actorID)
	require.NoError(t, err)

	require.Len(t, pub.events, 1)
	evt := pub.events[0]
	assert.Equal(t, domain.EventTenderAssigneeOverridden, evt.Type)
	assert.Equal(t, "198.51.100.23", evt.IPAddress)
	assert.Equal(t, "override-test-agent", evt.UserAgent)
	payload, ok := evt.Data.(domain.TenderAssigneeOverriddenPayload)
	require.True(t, ok)
	assert.Equal(t, tenderID, payload.TenderID)
	assert.Equal(t, newUserID, payload.UserID)
	assert.Equal(t, actorID, payload.ActorID)
}
