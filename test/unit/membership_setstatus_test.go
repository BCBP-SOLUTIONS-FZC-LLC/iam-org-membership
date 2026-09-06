// Unit tests for internal/core/service/membership_service.go SetStatus
// (P-7 suspend/reactivate). Covers:
//   - happy path (reactivate, no side-effects)
//   - suspend triggers RP RevokeUserSessions (AUTH-8)
//   - suspend triggers WFI-13 advisory (§8.8.5) — success + fail-open
//   - repo error short-circuits
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

// ── RealmProvisionerClient stub ────────────────────────────────────────

type fakeRPClient struct {
	revokeSessionsFn func(ctx context.Context, tenantID, kcUserID uuid.UUID) error
	revokeCalled     bool
	resetMFAFn       func(ctx context.Context, tenantID, kcUserID uuid.UUID) error
	resetMFACalled   bool
}

func (f *fakeRPClient) CreateInvitedUser(context.Context, port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	return nil, nil
}
func (f *fakeRPClient) DeleteUser(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *fakeRPClient) PatchRealmConfig(context.Context, uuid.UUID, port.RealmConfigPatch) error {
	return nil
}
func (f *fakeRPClient) RevokeUserSessions(ctx context.Context, tenantID, kcUserID uuid.UUID) error {
	f.revokeCalled = true
	if f.revokeSessionsFn == nil {
		return nil
	}
	return f.revokeSessionsFn(ctx, tenantID, kcUserID)
}
func (f *fakeRPClient) ResetMFA(ctx context.Context, tenantID, kcUserID uuid.UUID) error {
	f.resetMFACalled = true
	if f.resetMFAFn == nil {
		return nil
	}
	return f.resetMFAFn(ctx, tenantID, kcUserID)
}

var _ port.RealmProvisionerClient = (*fakeRPClient)(nil)

// ── Helper wire-up ─────────────────────────────────────────────────────

// noOwnerRoleRepo is a roles stub that reports the target user holds no
// tenant_owner grant — the common case for SetStatus tests. TM-13 fast-path
// is skipped when the user is not an owner, so no TxRunner is needed.
type noOwnerRoleRepo struct{}

func (noOwnerRoleRepo) ListByUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}
func (noOwnerRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (noOwnerRoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) { return 2, nil }
func (noOwnerRoleRepo) Grant(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
	return nil, nil
}
func (noOwnerRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (noOwnerRoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = noOwnerRoleRepo{}

// fakeMembershipRepo is a minimal MembershipRepository stub (only
// FindByUserID/SetStatus configurable) shared across several test files in
// this package.
type fakeMembershipRepo struct {
	findByUserIDFn func(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error)
	setStatusFn    func(ctx context.Context, tenantID, userID uuid.UUID, status domain.MembershipStatus, expectedVersion int64) (*domain.TenantMembership, error)
}

func (f *fakeMembershipRepo) List(ctx context.Context, tenantID uuid.UUID, cursor *domain.MembershipListCursor, limit int) (*domain.MembershipListPage, error) {
	return nil, errors.New("not used")
}
func (f *fakeMembershipRepo) FindByUserID(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error) {
	return f.findByUserIDFn(ctx, tenantID, userID)
}
func (f *fakeMembershipRepo) Insert(ctx context.Context, tm *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, errors.New("not used")
}
func (f *fakeMembershipRepo) SetStatus(ctx context.Context, tenantID, userID uuid.UUID, status domain.MembershipStatus, expectedVersion int64) (*domain.TenantMembership, error) {
	if f.setStatusFn == nil {
		return nil, errors.New("not used")
	}
	return f.setStatusFn(ctx, tenantID, userID, status, expectedVersion)
}
func (f *fakeMembershipRepo) SoftDelete(ctx context.Context, tenantID, userID uuid.UUID, expectedVersion int64) error {
	return errors.New("not used")
}
func (f *fakeMembershipRepo) CountActive(ctx context.Context, tenantID uuid.UUID) (int, error) {
	return 0, errors.New("not used")
}
func (f *fakeMembershipRepo) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*fakeMembershipRepo)(nil)

func buildMembershipSvcForSetStatus(m port.MembershipRepository, cache port.Cache, rp port.RealmProvisionerClient, wf port.WorkflowClient) *service.MembershipService {
	return service.NewMembershipService(m, noOwnerRoleRepo{}, nil, &port.TenantRepositoryNoop{}, nil, cache, rp, wf, nil, nil, 30)
}

// ── SetStatus — reactivate path (no side-effects) ──────────────────────

func TestMembership_SetStatus_ReactivateDoesNotCallRPOrWorkflow(t *testing.T) {
	m := &fakeMemRepoFull{}
	setStatusOnly := &fakeMembershipRepo{
		setStatusFn: func(_ context.Context, tt, uu uuid.UUID, st domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
			assert.Equal(t, domain.MembershipActive, st)
			return &domain.TenantMembership{ID: uuid.New(), TenantID: tt, UserID: uu, Status: st, RecordVersion: ver + 1}, nil
		},
	}
	rp := &fakeRPClient{}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			t.Fatal("workflow must NOT be consulted on reactivate")
			return nil, nil
		},
	}
	svc := buildMembershipSvcForSetStatus(setStatusOnly, nil, rp, wf)

	got, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipActive, 1)
	require.NoError(t, err)
	assert.Equal(t, domain.MembershipActive, got.Membership.Status)
	assert.False(t, rp.revokeCalled, "reactivate must not revoke sessions")
	assert.Nil(t, got.DelegateImpact, "no advisory on non-suspend paths")
	_ = m // silence unused
}

// ── SetStatus — repo error short-circuits (no RP/WF calls) ─────────────

func TestMembership_SetStatus_RepoErrorShortCircuits(t *testing.T) {
	repoErr := errors.New("optimistic_lock_conflict")
	m := &fakeMembershipRepo{
		setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
			return nil, repoErr
		},
	}
	rp := &fakeRPClient{}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			t.Fatal("workflow must NOT be called when SetStatus repo failed")
			return nil, nil
		},
	}
	svc := buildMembershipSvcForSetStatus(m, nil, rp, wf)

	_, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	assert.ErrorIs(t, err, repoErr)
	assert.False(t, rp.revokeCalled, "RP session revoke must not fire on repo failure")
}

// ── SetStatus — suspend triggers RP RevokeUserSessions ─────────────────

func TestMembership_SetStatus_SuspendCallsRPRevoke(t *testing.T) {
	m := &fakeMembershipRepo{
		setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipSuspended, RecordVersion: 2}, nil
		},
	}
	rp := &fakeRPClient{}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{}, nil
		},
	}
	svc := buildMembershipSvcForSetStatus(m, nil, rp, wf)

	_, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	require.NoError(t, err)
	assert.True(t, rp.revokeCalled, "AUTH-8: suspend must trigger RP RevokeUserSessions")
}

// ── SetStatus — WFI-13 advisory on suspend with active workflows ───────

func TestMembership_SetStatus_SuspendReturnsDelegateImpactAdvisory(t *testing.T) {
	m := &fakeMembershipRepo{
		setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipSuspended, RecordVersion: 2}, nil
		},
	}
	workflowID := uuid.New()
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(_ context.Context, _, _ uuid.UUID, delID *uuid.UUID) (*port.DelegateImpact, error) {
			assert.Nil(t, delID, "P-7 suspend uses tenant-wide impact (no delegation scope)")
			return &port.DelegateImpact{ActiveWorkflows: 4, WorkflowIDs: []uuid.UUID{workflowID}}, nil
		},
	}
	svc := buildMembershipSvcForSetStatus(m, nil, &fakeRPClient{}, wf)

	got, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	require.NoError(t, err)
	require.NotNil(t, got.DelegateImpact, "WFI-13: suspend must surface delegate-impact advisory")
	assert.True(t, got.DelegateImpact.Checked)
	assert.Equal(t, 4, got.DelegateImpact.ActiveWorkflows)
	assert.Contains(t, got.DelegateImpact.WorkflowIDs, workflowID)
}

// ── SetStatus — suspend with no active workflows returns nil advisory ─

func TestMembership_SetStatus_SuspendWithNoActiveWorkflowsReturnsNilAdvisory(t *testing.T) {
	m := &fakeMembershipRepo{
		setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipSuspended, RecordVersion: 2}, nil
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return &port.DelegateImpact{ActiveWorkflows: 0}, nil
		},
	}
	svc := buildMembershipSvcForSetStatus(m, nil, &fakeRPClient{}, wf)

	got, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	require.NoError(t, err)
	assert.Nil(t, got.DelegateImpact, "no advisory when active_workflows == 0")
}

// ── SetStatus — workflow-service failure is fail-open (Checked=false) ──

func TestMembership_SetStatus_SuspendWorkflowFailureReturnsUncheckedAdvisory(t *testing.T) {
	m := &fakeMembershipRepo{
		setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipSuspended, RecordVersion: 2}, nil
		},
	}
	wf := &fakeWorkflowClient{
		getDelegateImpactFn: func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
			return nil, errors.New("workflow svc unavailable")
		},
	}
	svc := buildMembershipSvcForSetStatus(m, nil, &fakeRPClient{}, wf)

	got, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	require.NoError(t, err, "WFI-13: workflow-svc failure must NOT surface — suspend is authoritative")
	require.NotNil(t, got.DelegateImpact)
	assert.False(t, got.DelegateImpact.Checked,
		"fail-open: Checked=false signals the advisory couldn't be produced")
}

// ── SetStatus — nil workflow client is safe (advisory skipped entirely) ─

func TestMembership_SetStatus_SuspendWithNilWorkflowClientSkipsAdvisory(t *testing.T) {
	m := &fakeMembershipRepo{
		setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipSuspended, RecordVersion: 2}, nil
		},
	}
	svc := buildMembershipSvcForSetStatus(m, nil, &fakeRPClient{}, nil)

	got, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	require.NoError(t, err)
	assert.Nil(t, got.DelegateImpact, "workflow=nil path skips advisory entirely")
}

// ── SetStatus — TM-8 last-owner guard + TM-13 lock (Bug B-10 fix) ─────

// ownerRoleRepo returns a single tenant_owner grant for the queried user,
// simulating the target being the (last) owner.
type ownerRoleRepo struct {
	countFn func(context.Context, uuid.UUID) (int, error)
}

func (r *ownerRoleRepo) ListByUser(_ context.Context, tid, uid uuid.UUID) ([]domain.TenantRole, error) {
	return []domain.TenantRole{{TenantID: tid, UserID: uid, RoleCode: domain.RoleTenantOwner}}, nil
}
func (r *ownerRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (r *ownerRoleRepo) CountActiveOwners(ctx context.Context, tid uuid.UUID) (int, error) {
	if r.countFn != nil {
		return r.countFn(ctx, tid)
	}
	return 1, nil
}
func (r *ownerRoleRepo) Grant(context.Context, *domain.TenantRole) (*domain.TenantRole, error) {
	return nil, nil
}
func (r *ownerRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (r *ownerRoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*ownerRoleRepo)(nil)

// TM-8: suspending the LAST tenant_owner → 422 last_owner_removal.
func TestMembership_SetStatus_SuspendLastOwner_422LastOwnerRemoval(t *testing.T) {
	roles := &ownerRoleRepo{countFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil }}
	svc := service.NewMembershipService(
		&fakeMembershipRepo{}, roles, nil, &port.TenantRepositoryNoop{}, nil,
		nil, &fakeRPClient{}, nil, &ruTxRunner{}, nil, 30,
	)

	_, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrLastOwnerRemoval, de.Cause)
}

// TM-8: suspending an owner when ≥2 owners exist → allowed (200).
func TestMembership_SetStatus_SuspendOwnerWithOtherOwnersAllowed(t *testing.T) {
	roles := &ownerRoleRepo{countFn: func(context.Context, uuid.UUID) (int, error) { return 2, nil }}
	m := &fakeMembershipRepo{
		setStatusFn: func(_ context.Context, tid, uid uuid.UUID, s domain.MembershipStatus, ver int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{TenantID: tid, UserID: uid, Status: s, RecordVersion: ver + 1}, nil
		},
	}
	svc := service.NewMembershipService(
		m, roles, nil, &port.TenantRepositoryNoop{}, nil,
		nil, &fakeRPClient{}, nil, &ruTxRunner{}, nil, 30,
	)

	got, err := svc.SetStatus(context.Background(), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	require.NoError(t, err)
	assert.Equal(t, domain.MembershipSuspended, got.Membership.Status)
}

// ── SetStatus — cache invalidation happens on success ──────────────────

func TestMembership_SetStatus_InvalidatesMemberCacheOnSuccess(t *testing.T) {
	tenantID := uuid.New()
	m := &fakeMembershipRepo{
		setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New()}, nil
		},
	}
	cache := &spyCache{}
	svc := buildMembershipSvcForSetStatus(m, cache, &fakeRPClient{}, nil)

	_, err := svc.SetStatus(context.Background(), tenantID, uuid.New(), domain.MembershipActive, 1)
	require.NoError(t, err)
	assert.Contains(t, cache.deleteCalls, "om:members:"+tenantID.String()+":50")
	assert.Contains(t, cache.deleteCalls, "om:seat_usage:"+tenantID.String())
}
