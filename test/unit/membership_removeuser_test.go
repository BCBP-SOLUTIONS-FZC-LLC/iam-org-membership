// Full-cascade unit tests for MembershipService.RemoveUser (P-8 §8.8).
//
// Uses dedicated stubs (all methods scripted) plus a passthrough TxRunner
// that injects a fakeTx via service.WithTx AND a fake event publisher via
// port.WithEventPublisher — mirroring the production postgres.TxRunner.
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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── fakeTx exposing Exec (for the TM-13 FOR UPDATE lock) ──────────────

type ruFakeTx struct {
	pgx.Tx
	execFn func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (f *ruFakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.execFn != nil {
		return f.execFn(ctx, sql, args...)
	}
	return pgconn.CommandTag{}, nil
}

// ── injecting TxRunner ─────────────────────────────────────────────────

type ruTxRunner struct {
	tx  pgx.Tx
	pub port.ContextEventPublisher
}

func (r *ruTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	ctx = service.WithTx(ctx, r.tx)
	if r.pub != nil {
		ctx = port.WithEventPublisher(ctx, r.pub)
	}
	return fn(ctx)
}

// ── recording event publisher ──────────────────────────────────────────

type ruPublisher struct {
	events []*domain.DomainEvent
}

func (p *ruPublisher) EnqueueCtx(_ context.Context, e *domain.DomainEvent) error {
	p.events = append(p.events, e)
	return nil
}

// ── comprehensive fakes (only the methods RemoveUser touches) ──────────

type ruMembershipRepo struct {
	findByUserIDFn func(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error)
	softDeleteFn   func(ctx context.Context, tenantID, userID uuid.UUID, expectedVersion int64) error
}

func (f *ruMembershipRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (f *ruMembershipRepo) FindByUserID(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error) {
	return f.findByUserIDFn(ctx, tenantID, userID)
}
func (f *ruMembershipRepo) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *ruMembershipRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *ruMembershipRepo) SoftDelete(ctx context.Context, tenantID, userID uuid.UUID, expectedVersion int64) error {
	return f.softDeleteFn(ctx, tenantID, userID, expectedVersion)
}
func (f *ruMembershipRepo) CountActive(context.Context, uuid.UUID) (int, error) { return 0, nil }

type ruRoleRepo struct {
	listByUserFn           func(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error)
	countActiveOwnersFn    func(ctx context.Context, tenantID uuid.UUID) (int, error)
	softDeleteAllForUserFn func(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error)
}

func (f *ruRoleRepo) ListByUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error) {
	return f.listByUserFn(ctx, tenantID, userID)
}
func (f *ruRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (f *ruRoleRepo) CountActiveOwners(ctx context.Context, tenantID uuid.UUID) (int, error) {
	return f.countActiveOwnersFn(ctx, tenantID)
}
func (f *ruRoleRepo) Grant(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
	return nil, nil
}
func (f *ruRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (f *ruRoleRepo) SoftDeleteAllForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error) {
	return f.softDeleteAllForUserFn(ctx, tenantID, userID)
}

type ruDeptMemRepo struct {
	softDeleteAllForUserFn func(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.DeptMembership, error)
}

func (f *ruDeptMemRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (f *ruDeptMemRepo) ListByDepartment(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}
func (f *ruDeptMemRepo) Assign(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, domain.DeptRole, uuid.UUID) (*domain.DeptMembership, *domain.DeptMembership, error) {
	return nil, nil, nil
}
func (f *ruDeptMemRepo) Remove(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
	return nil, nil
}
func (f *ruDeptMemRepo) SoftDeleteAllForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.DeptMembership, error) {
	return f.softDeleteAllForUserFn(ctx, tenantID, userID)
}
func (f *ruDeptMemRepo) SoftDeleteAllForDept(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
	return nil, nil
}

type ruDelegationRepo struct {
	softDeleteForUserFn func(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.Delegation, error)
}

func (f *ruDelegationRepo) List(context.Context, uuid.UUID) ([]domain.Delegation, error) {
	return nil, nil
}
func (f *ruDelegationRepo) ListByDelegator(context.Context, uuid.UUID, uuid.UUID) ([]domain.Delegation, error) {
	return nil, nil
}
func (f *ruDelegationRepo) FindByID(context.Context, uuid.UUID, uuid.UUID) (*domain.Delegation, error) {
	return nil, nil
}
func (f *ruDelegationRepo) Insert(context.Context, *domain.Delegation) (*domain.Delegation, error) {
	return nil, nil
}
func (f *ruDelegationRepo) End(context.Context, uuid.UUID, uuid.UUID, domain.DelegationStatus, int64) (*domain.Delegation, error) {
	return nil, nil
}
func (f *ruDelegationRepo) ListExpiringBefore(context.Context, time.Time, int) ([]domain.Delegation, error) {
	return nil, nil
}
func (f *ruDelegationRepo) SoftDeleteForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.Delegation, error) {
	return f.softDeleteForUserFn(ctx, tenantID, userID)
}
func (f *ruDelegationRepo) FindActiveDeptDelegateForUser(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.Delegation, error) {
	return nil, nil
}
func (f *ruDelegationRepo) ExtendReview(context.Context, uuid.UUID, uuid.UUID, int, int64) (*domain.Delegation, error) {
	return nil, nil
}
func (f *ruDelegationRepo) FindOpenEndedForReview(context.Context, time.Time, int) ([]domain.Delegation, error) {
	return nil, nil
}
func (f *ruDelegationRepo) FindOpenEndedForWarning(context.Context, time.Time, int) ([]domain.Delegation, error) {
	return nil, nil
}
func (f *ruDelegationRepo) MarkReviewNoticeSent(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}

type ruACLRepo struct {
	softDeleteForUserFn func(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenderACLEntry, error)
}

func (f *ruACLRepo) ListByTender(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error) {
	return nil, nil
}
func (f *ruACLRepo) FindActiveForUser(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.TenderACLEntry, error) {
	return nil, nil
}
func (f *ruACLRepo) Grant(context.Context, *domain.TenderACLEntry) (*domain.TenderACLEntry, error) {
	return nil, nil
}
func (f *ruACLRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.TenderACLEntry, error) {
	return nil, nil
}
func (f *ruACLRepo) SoftDeleteForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenderACLEntry, error) {
	return f.softDeleteForUserFn(ctx, tenantID, userID)
}

type ruRPClient struct {
	revokeCalled bool
	revokeErr    error
}

func (r *ruRPClient) CreateInvitedUser(context.Context, port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	return nil, nil
}
func (r *ruRPClient) DeleteUser(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (r *ruRPClient) PatchRealmConfig(context.Context, uuid.UUID, port.RealmConfigPatch) error {
	return nil
}
func (r *ruRPClient) RevokeUserSessions(context.Context, uuid.UUID, uuid.UUID) error {
	r.revokeCalled = true
	return r.revokeErr
}

// buildRemoveUserSvc wires MembershipService with the RemoveUser
// collaborators only. All others stay nil.
func buildRemoveUserSvc(
	m port.MembershipRepository, r port.TenantRoleRepository, dm port.DeptMembershipRepository,
	del port.DelegationRepository, acls port.TenderACLRepository,
	rp port.RealmProvisionerClient, wf port.WorkflowClient, tr port.TxRunner,
) *service.MembershipService {
	return service.NewMembershipService(m, r, dm, del, acls, nil, nil, nil, rp, wf, tr, nil, 30)
}

// happyRemoveUserFakes returns fakes primed for a fully-successful cascade.
type removeUserSetup struct {
	svc *service.MembershipService
	pub *ruPublisher
	rp  *ruRPClient
	tx  *ruFakeTx
}

func setupHappyRemoveUser(t *testing.T, tenantID, userID uuid.UUID) *removeUserSetup {
	t.Helper()
	deptID := uuid.New()
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), UserID: userID, RecordVersion: 3}, nil
		},
		softDeleteFn: func(_ context.Context, tt, uu uuid.UUID, ver int64) error {
			assert.Equal(t, tenantID, tt)
			assert.Equal(t, userID, uu)
			assert.EqualValues(t, 3, ver, "SoftDelete must pass the read-side record_version for CONC-1")
			return nil
		},
	}
	r := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) { return 0, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
	}
	dm := &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return []domain.DeptMembership{{DepartmentID: deptID}}, nil
		},
	}
	del := &ruDelegationRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.Delegation, error) {
			return []domain.Delegation{{ID: uuid.New(), DelegatorID: userID, DelegateID: uuid.New(), Scope: domain.ScopeAll}}, nil
		},
	}
	acls := &ruACLRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error) {
			return nil, nil
		},
	}
	rp := &ruRPClient{}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	tx := &ruFakeTx{}
	pub := &ruPublisher{}
	tr := &ruTxRunner{tx: tx, pub: pub}
	svc := buildRemoveUserSvc(m, r, dm, del, acls, rp, wf, tr)

	return &removeUserSetup{svc: svc, pub: pub, rp: rp, tx: tx}
}

// ── Happy path — full cascade emits Revoked events + calls RP revoke ──

func TestMembership_RemoveUser_HappyPathEmitsRevokedEventsAndCallsRPRevoke(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	s := setupHappyRemoveUser(t, tenantID, userID)

	err := s.svc.RemoveUser(context.Background(), tenantID, userID, uuid.New())
	require.NoError(t, err)

	// TenantRoleRevoked + DepartmentMembershipRevoked + DelegationEnded.
	require.Len(t, s.pub.events, 3)
	assert.Equal(t, domain.EventTenantRoleRevoked, s.pub.events[0].Type)
	assert.Equal(t, domain.EventDepartmentMembershipRevoked, s.pub.events[1].Type)
	assert.Equal(t, domain.EventDelegationEnded, s.pub.events[2].Type)

	// AUTH-8 privilege reduction: RP RevokeUserSessions fired best-effort.
	assert.True(t, s.rp.revokeCalled, "AUTH-8: post-commit session revoke")
}

// ── Owner-only path is refused when they'd be the last owner ────────────

func TestMembership_RemoveUser_LastOwnerRefused(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	r := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) {
			return 1, nil
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	svc := buildRemoveUserSvc(m, r, nil, nil, nil, nil, wf, &ruTxRunner{tx: &ruFakeTx{}})

	err := svc.RemoveUser(context.Background(), tenantID, userID, uuid.New())
	assert.ErrorIs(t, err, domain.ErrLastOwnerRemoval)
}

// ── Owner removal with additional owners proceeds ──────────────────────

func TestMembership_RemoveUser_OwnerWithOtherOwnersProceeds(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	r := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 3, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	dm := &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil },
	}
	del := &ruDelegationRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.Delegation, error) { return nil, nil },
	}
	acls := &ruACLRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error) { return nil, nil },
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	svc := buildRemoveUserSvc(m, r, dm, del, acls, &ruRPClient{}, wf, &ruTxRunner{tx: &ruFakeTx{}})

	err := svc.RemoveUser(context.Background(), tenantID, userID, uuid.New())
	assert.NoError(t, err)
}

// ── Tenant lock error propagates ────────────────────────────────────────

func TestMembership_RemoveUser_TenantLockErrorPropagates(t *testing.T) {
	lockErr := errors.New("lock timeout")
	tx := &ruFakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, lockErr
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	svc := buildRemoveUserSvc(nil, nil, nil, nil, nil, nil, wf, &ruTxRunner{tx: tx})
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, lockErr)
}

// ── Membership lookup failure inside tx propagates ─────────────────────

func TestMembership_RemoveUser_MembershipNotFoundInsideTx(t *testing.T) {
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return nil, domain.NewError(domain.ErrMemberNotFound, "gone")
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	svc := buildRemoveUserSvc(m, nil, nil, nil, nil, nil, wf, &ruTxRunner{tx: &ruFakeTx{}})
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

// ── nil workflow client skips pre-check entirely ────────────────────────

func TestMembership_RemoveUser_NilWorkflowSkipsPreCheck(t *testing.T) {
	// workflow=nil → no WFI-3 pre-check, proceed straight to tx.
	tenantID, userID := uuid.New(), uuid.New()
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	r := &ruRoleRepo{
		listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 5, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	dm := &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil },
	}
	del := &ruDelegationRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.Delegation, error) { return nil, nil },
	}
	acls := &ruACLRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error) { return nil, nil },
	}
	svc := buildRemoveUserSvc(m, r, dm, del, acls, nil, nil, &ruTxRunner{tx: &ruFakeTx{}})

	err := svc.RemoveUser(context.Background(), tenantID, userID, uuid.New())
	assert.NoError(t, err)
}

// ── tx-unavailable branch ──────────────────────────────────────────────

func TestMembership_RemoveUser_TxUnavailable(t *testing.T) {
	// txRunner passes ctx WITHOUT injecting a tx → pgadapterTxFromContext
	// finds nothing → ErrConflict.
	tr := &passthroughTxRunner{} // from dept_membership_service_test — doesn't inject tx
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	svc := buildRemoveUserSvc(nil, nil, nil, nil, nil, nil, wf, tr)
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrConflict)
}

// ── cascade error propagates ─ role.SoftDeleteAllForUser fails ─────────

func TestMembership_RemoveUser_RolesCascadeErrorPropagates(t *testing.T) {
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	rolesErr := errors.New("roles cascade blew up")
	r := &ruRoleRepo{
		listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 5, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, rolesErr },
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	svc := buildRemoveUserSvc(m, r, nil, nil, nil, nil, wf, &ruTxRunner{tx: &ruFakeTx{}})
	err := svc.RemoveUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, rolesErr)
}

// ── P8-HP-02: delegator direction also emits DelegationEnded ──────────────
// DEL-7 / §15.2.2 step 3: SoftDeleteForUser ends delegations where the user
// is the DELEGATOR. The event must be emitted regardless of which side the
// removed user is on.

func TestMembership_RemoveUser_DelegatorDirectionCovered(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	s := setupHappyRemoveUser(t, tenantID, userID)

	// Override the delegation repo so the removed user is the DELEGATOR (not
	// the delegate). The cascade SQL uses OR — both directions are deleted.
	delegationID := uuid.New()
	delegateID := uuid.New()
	s.svc = buildRemoveUserSvc(
		&ruMembershipRepo{
			findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
				return &domain.TenantMembership{ID: uuid.New(), UserID: userID, RecordVersion: 1}, nil
			},
			softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
		},
		&ruRoleRepo{
			listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
			countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 5, nil },
			softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		},
		&ruDeptMemRepo{
			softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil },
		},
		&ruDelegationRepo{
			softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.Delegation, error) {
				// User is the DELEGATOR on this delegation.
				return []domain.Delegation{{
					ID:          delegationID,
					DelegatorID: userID, // removed user is delegator
					DelegateID:  delegateID,
					Scope:       domain.ScopeAll,
				}}, nil
			},
		},
		&ruACLRepo{softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error) { return nil, nil }},
		s.rp,
		&fakeWorkflowClient{getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		}},
		&ruTxRunner{tx: s.tx, pub: s.pub},
	)

	err := s.svc.RemoveUser(context.Background(), tenantID, userID, uuid.New())
	require.NoError(t, err)

	// DelegationEnded must be emitted even when the removed user is the delegator.
	require.Len(t, s.pub.events, 1)
	assert.Equal(t, domain.EventDelegationEnded, s.pub.events[0].Type)
	payload, ok := s.pub.events[0].Data.(domain.DelegationEndedPayload)
	require.True(t, ok)
	assert.Equal(t, domain.EndReasonDelegateRemoved, payload.EndedReason)
	assert.Equal(t, delegationID, payload.DelegationID)
}

// ── P8-EDGE-01: tenant_admin self-removal succeeds (no self-removal prohibition) ─
// LLD §8.8: no prohibition on a tenant_admin removing their own membership.
// TM-8 is not triggered (non-owner). Cascade runs → 204.

func TestMembership_RemoveUser_SelfRemovalAsAdmin(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	// actorID == userID → self-removal
	actorID := userID

	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	r := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
		countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 3, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	dm := &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil },
	}
	del := &ruDelegationRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.Delegation, error) { return nil, nil },
	}
	acls := &ruACLRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error) { return nil, nil },
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	pub := &ruPublisher{}
	tr := &ruTxRunner{tx: &ruFakeTx{}, pub: pub}
	svc := buildRemoveUserSvc(m, r, dm, del, acls, &ruRPClient{}, wf, tr)

	err := svc.RemoveUser(context.Background(), tenantID, userID, actorID)
	assert.NoError(t, err, "tenant_admin self-removal must succeed — no self-removal prohibition in LLD")
}

// ── P8-EVT-01: 1 role + 2 depts + 1 delegation → 4 events in order ───────
// EVT-10 / TR-9 / DEL-7 / §16 A45: exact event count and sequence verified.

func TestMembership_RemoveUser_EventOrderTwoDeptsOneDelegation(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	dept1, dept2 := uuid.New(), uuid.New()
	pub := &ruPublisher{}
	tx := &ruFakeTx{}
	tr := &ruTxRunner{tx: tx, pub: pub}

	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	r := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) { return 5, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantAdmin}}, nil
		},
	}
	dm := &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) {
			return []domain.DeptMembership{
				{DepartmentID: dept1},
				{DepartmentID: dept2},
			}, nil
		},
	}
	del := &ruDelegationRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.Delegation, error) {
			return []domain.Delegation{{ID: uuid.New(), DelegatorID: uuid.New(), DelegateID: userID, Scope: domain.ScopeAll}}, nil
		},
	}
	acls := &ruACLRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error) { return nil, nil },
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	svc := buildRemoveUserSvc(m, r, dm, del, acls, &ruRPClient{}, wf, tr)

	err := svc.RemoveUser(context.Background(), tenantID, userID, uuid.New())
	require.NoError(t, err)

	// Exactly 4 events: TenantRoleRevoked×1, DeptMembershipRevoked×2, DelegationEnded×1.
	require.Len(t, pub.events, 4)
	assert.Equal(t, domain.EventTenantRoleRevoked, pub.events[0].Type)
	assert.Equal(t, domain.EventDepartmentMembershipRevoked, pub.events[1].Type)
	assert.Equal(t, domain.EventDepartmentMembershipRevoked, pub.events[2].Type)
	assert.Equal(t, domain.EventDelegationEnded, pub.events[3].Type)
}

// ── P26-EVT-01: 2 delegations → 2 DelegationEnded events (both enqueued in tx) ─
// EVT-10 atomicity: all DelegationEnded events emitted in same RunInTx as the
// soft-deletes. Tests the N > 1 delegation path (DEL-7).

func TestMembership_RemoveUser_MultiDelegationsAllEmitDelegationEnded(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	del1, del2 := uuid.New(), uuid.New()
	pub := &ruPublisher{}
	tr := &ruTxRunner{tx: &ruFakeTx{}, pub: pub}

	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	r := &ruRoleRepo{
		listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 5, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	dm := &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil },
	}
	del := &ruDelegationRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.Delegation, error) {
			// Two delegations — one as delegate (all scope), one as delegator.
			return []domain.Delegation{
				{ID: del1, DelegatorID: uuid.New(), DelegateID: userID, Scope: domain.ScopeAll},
				{ID: del2, DelegatorID: userID, DelegateID: uuid.New(), Scope: domain.ScopeDepartment},
			}, nil
		},
	}
	acls := &ruACLRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error) { return nil, nil },
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	svc := buildRemoveUserSvc(m, r, dm, del, acls, &ruRPClient{}, wf, tr)

	err := svc.RemoveUser(context.Background(), tenantID, userID, uuid.New())
	require.NoError(t, err)

	// Exactly 2 DelegationEnded events, one per soft-deleted delegation row.
	delegationEvents := 0
	for _, e := range pub.events {
		if e.Type == domain.EventDelegationEnded {
			delegationEvents++
			payload, ok := e.Data.(domain.DelegationEndedPayload)
			require.True(t, ok)
			assert.Equal(t, domain.EndReasonDelegateRemoved, payload.EndedReason)
		}
	}
	assert.Equal(t, 2, delegationEvents, "DelegationEnded must be emitted for every soft-deleted delegation row (both directions)")
}

// ── P8-CONC-01: second concurrent owner-drop sees owners==1 inside lock → 422 ─
// TM-13: SELECT FOR UPDATE serializes concurrent owner-drops. The re-check
// inside the lock uses CountActiveOwners — the losing request finds owners==1
// and must return ErrLastOwnerRemoval (same invariant as last-owner refusal).

func TestMembership_RemoveUser_ConcurrentOwnerDropSecondRequestSees1(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
	}
	r := &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{RoleCode: domain.RoleTenantOwner}}, nil
		},
		// Inside the FOR UPDATE lock the count has dropped to 1 — simulates the
		// concurrent first request already committed its owner-removal.
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil },
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	svc := buildRemoveUserSvc(m, r, nil, nil, nil, nil, wf, &ruTxRunner{tx: &ruFakeTx{}})

	err := svc.RemoveUser(context.Background(), tenantID, userID, uuid.New())
	assert.ErrorIs(t, err, domain.ErrLastOwnerRemoval,
		"TM-13: second concurrent request sees owners==1 inside the lock and must be refused")
}

// ── P8-EVT-02: plain member (no tenant_roles row) → zero TenantRoleRevoked ─
// TR-7: member role is derived; SoftDeleteAllForUser returns empty slice;
// no TenantRoleRevoked event must be enqueued.

func TestMembership_RemoveUser_PlainMember_NoRoleRevokedEvents(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	pub := &ruPublisher{}
	tr := &ruTxRunner{tx: &ruFakeTx{}, pub: pub}

	m := &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
	r := &ruRoleRepo{
		// Plain member has no elevated roles in tenant_roles table (TR-7).
		listByUserFn:           func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
		countActiveOwnersFn:    func(context.Context, uuid.UUID) (int, error) { return 5, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) { return nil, nil },
	}
	dm := &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil },
	}
	del := &ruDelegationRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.Delegation, error) { return nil, nil },
	}
	acls := &ruACLRepo{
		softDeleteForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenderACLEntry, error) { return nil, nil },
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	svc := buildRemoveUserSvc(m, r, dm, del, acls, &ruRPClient{}, wf, tr)

	err := svc.RemoveUser(context.Background(), tenantID, userID, uuid.New())
	require.NoError(t, err)

	for _, e := range pub.events {
		assert.NotEqual(t, domain.EventTenantRoleRevoked, e.Type,
			"TR-7: plain member has no tenant_roles row; TenantRoleRevoked must not be emitted")
	}
}
