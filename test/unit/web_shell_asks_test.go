// Tests for the web-shell IAM-screens asks (2026-11-24): OM-1
// member_suspended, OM-2 read_only_until, OM-6 owner-only admin roles.
package unit_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
)

// ── OM-2: read_only_until ───────────────────────────────────────────────────

func TestTenant_ReadOnlyUntil(t *testing.T) {
	cancelled := time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC)
	tn := &domain.Tenant{Status: domain.StatusCancelled, CancelledAt: &cancelled}

	got := tn.ReadOnlyUntil(30)
	require.NotNil(t, got)
	assert.Equal(t, cancelled.AddDate(0, 0, 30), *got)

	got = tn.ReadOnlyUntil(0)
	require.NotNil(t, got, "0 falls back to the default grace window")
	assert.Equal(t, cancelled.AddDate(0, 0, domain.DefaultSubscriptionGraceDays), *got)

	got = tn.ReadOnlyUntil(7)
	require.NotNil(t, got)
	assert.Equal(t, cancelled.AddDate(0, 0, 7), *got)
}

func TestTenant_ReadOnlyUntil_NilOutsideCancelled(t *testing.T) {
	at := time.Now()
	for _, st := range []domain.SubscriptionStatus{
		domain.StatusTrial, domain.StatusActive, domain.StatusPastDue,
		domain.StatusSuspended, domain.StatusTrialExpired, domain.StatusOffboarded,
	} {
		tn := &domain.Tenant{Status: st, CancelledAt: &at}
		assert.Nil(t, tn.ReadOnlyUntil(30), st)
	}
	assert.Nil(t, (&domain.Tenant{Status: domain.StatusCancelled}).ReadOnlyUntil(30), "no cancelled_at")
	assert.Nil(t, (*domain.Tenant)(nil).ReadOnlyUntil(30))
}

func TestTenantService_ReadOnlyUntil_UsesConfiguredGrace(t *testing.T) {
	cancelled := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	tn := &domain.Tenant{Status: domain.StatusCancelled, CancelledAt: &cancelled}
	svc := service.NewTenantService(nil, nil, nil).WithSubscriptionGraceDays(14)
	got := svc.ReadOnlyUntil(tn)
	require.NotNil(t, got)
	assert.Equal(t, cancelled.AddDate(0, 0, 14), *got)
}

func TestSessionContext_ReadOnlyUntil_CancelledTenant(t *testing.T) {
	cancelled := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	tenants := scTenant()
	tenants.tenant.Status = domain.StatusCancelled
	tenants.tenant.CancelledAt = &cancelled
	svc := scService(scRow(domain.MembershipActive, domain.StatusCancelled), tenants).WithSubscriptionGraceDays(30)

	sc, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.True(t, sc.ReadOnly)
	require.NotNil(t, sc.ReadOnlyUntil)
	assert.Equal(t, cancelled.AddDate(0, 0, 30), *sc.ReadOnlyUntil)
}

// The I-8 projection (300 s TTL) can lag om:tenant just after a
// cancellation: no read_only_until while read_only is still false.
func TestSessionContext_ReadOnlyUntil_FollowsProjectionReadOnly(t *testing.T) {
	cancelled := time.Now().UTC()
	tenants := scTenant()
	tenants.tenant.Status = domain.StatusCancelled
	tenants.tenant.CancelledAt = &cancelled
	svc := scService(scRow(domain.MembershipActive, domain.StatusActive), tenants)
	sc, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.False(t, sc.ReadOnly)
	assert.Nil(t, sc.ReadOnlyUntil)
}

func TestSessionContext_ReadOnlyUntil_NilWhenActive(t *testing.T) {
	svc := scService(scRow(domain.MembershipActive, domain.StatusActive), scTenant())
	sc, err := svc.Get(context.Background(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Nil(t, sc.ReadOnlyUntil)
}

// ── OM-6: only a tenant_owner may grant or revoke tenant_owner/tenant_admin ─

func om6Service(current []domain.TenantRoleCode) (*service.MembershipService, *rrPub) {
	mem := &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipActive}
	roles := &extRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			out := make([]domain.TenantRole, len(current))
			for i, c := range current {
				out[i] = domain.TenantRole{RoleCode: c}
			}
			return out, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) { return 2, nil },
	}
	m := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return mem, nil
	}}
	pub := &rrPub{}
	return service.NewMembershipService(m, roles, nil, &port.TenantRepositoryNoop{}, nil, nil, &fakeRPClient{}, nil, &rrTxRunner{pub: pub}, nil), pub
}

func callerCtx(roles ...string) context.Context {
	return requestctx.WithContext(context.Background(), &requestctx.RequestContext{
		UserID: uuid.New(), TenantID: uuid.New(), Roles: roles,
	})
}

func TestReconcileRoles_AdminCannotChangeAdminRoles(t *testing.T) {
	cases := []struct {
		name    string
		current []domain.TenantRoleCode
		desired []domain.TenantRoleCode
	}{
		{"grant tenant_admin", nil, []domain.TenantRoleCode{domain.RoleTenantAdmin}},
		{"grant tenant_owner", nil, []domain.TenantRoleCode{domain.RoleTenantOwner}},
		{"revoke tenant_admin", []domain.TenantRoleCode{domain.RoleTenantAdmin}, nil},
		{"revoke tenant_owner", []domain.TenantRoleCode{domain.RoleTenantOwner}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, pub := om6Service(tc.current)
			_, _, err := svc.ReconcileRoles(callerCtx("tenant_admin"), uuid.New(), uuid.New(), tc.desired, uuid.New())
			require.ErrorIs(t, err, domain.ErrInsufficientRole)
			assert.Empty(t, pub.events, "no change committed")
		})
	}
}

func TestReconcileRoles_AdminMayChangeTenderAdminAlone(t *testing.T) {
	// The target keeps tenant_admin; only tender_admin is added.
	svc, pub := om6Service([]domain.TenantRoleCode{domain.RoleTenantAdmin})
	granted, _, err := svc.ReconcileRoles(callerCtx("tenant_admin"), uuid.New(), uuid.New(),
		[]domain.TenantRoleCode{domain.RoleTenantAdmin, domain.RoleTenderAdmin}, uuid.New())
	require.NoError(t, err)
	require.Len(t, granted, 1)
	assert.Equal(t, domain.RoleTenderAdmin, granted[0].RoleCode)
	assert.Len(t, pub.events, 1)
}

func TestReconcileRoles_OwnerMayChangeAdminRoles(t *testing.T) {
	svc, _ := om6Service([]domain.TenantRoleCode{domain.RoleTenderAdmin})
	granted, revoked, err := svc.ReconcileRoles(callerCtx("tenant_owner"), uuid.New(), uuid.New(),
		[]domain.TenantRoleCode{domain.RoleTenantAdmin}, uuid.New())
	require.NoError(t, err)
	assert.Len(t, granted, 1)
	assert.Len(t, revoked, 1)
}

func TestInvite_AdminCannotInviteIntoAdminRoles(t *testing.T) {
	svc := buildInvitationSvc(nil, nil) // the guard runs before any repo call
	for _, role := range []domain.TenantRoleCode{domain.RoleTenantAdmin, domain.RoleTenantOwner} {
		_, err := svc.Invite(callerCtx("tenant_admin"), uuid.New(), service.InvitationInput{
			Email: "new@example.com", FullName: "New Person",
			InitialTenantRoles: []domain.TenantRoleCode{role},
		}, uuid.New())
		require.ErrorIs(t, err, domain.ErrInsufficientRole, role)
	}
}

// ── HLD §6.4 extended to lifecycle actions: P-7 suspend and P-8 remove
//    are owner-only on a privileged target ─────────────────────────────

func lifecycleRoleRepo(target []domain.TenantRoleCode) *ruRoleRepo {
	return &ruRoleRepo{
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			out := make([]domain.TenantRole, len(target))
			for i, c := range target {
				out[i] = domain.TenantRole{RoleCode: c}
			}
			return out, nil
		},
		countActiveOwnersFn: func(context.Context, uuid.UUID) (int, error) { return 2, nil },
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, nil
		},
	}
}

type countingWorkflow struct{ impact, cancel int }

func (w *countingWorkflow) GetDelegateImpact(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) (*port.DelegateImpact, error) {
	w.impact++
	return &port.DelegateImpact{}, nil
}
func (w *countingWorkflow) ReassignDelegate(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, *uuid.UUID) error {
	return nil
}
func (w *countingWorkflow) CancelByDelegate(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID) error {
	w.cancel++
	return nil
}

func lifecycleMembers() *ruMembershipRepo {
	return &ruMembershipRepo{
		findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipActive, RecordVersion: 1}, nil
		},
		softDeleteFn: func(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil },
	}
}

func TestRemoveUser_AdminCannotRemovePrivilegedMember(t *testing.T) {
	for _, role := range []domain.TenantRoleCode{domain.RoleTenantAdmin, domain.RoleTenantOwner} {
		wf := &countingWorkflow{}
		pub := &ruPublisher{}
		svc := buildRemoveUserSvc(lifecycleMembers(), lifecycleRoleRepo([]domain.TenantRoleCode{role}), &ruDeptMemRepo{}, &ruRPClient{}, wf, &ruTxRunner{pub: pub})
		err := svc.RemoveUser(callerCtx("tenant_admin"), uuid.New(), uuid.New(), uuid.New())
		require.ErrorIs(t, err, domain.ErrInsufficientRole, role)
		assert.Zero(t, wf.impact, "refused before the delegate-impact call")
		assert.Empty(t, pub.events, role)
	}
}

func TestRemoveUser_AdminMayRemoveNonPrivilegedMember(t *testing.T) {
	wf := &countingWorkflow{}
	svc := buildRemoveUserSvc(lifecycleMembers(), lifecycleRoleRepo([]domain.TenantRoleCode{domain.RoleTenderAdmin}), &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil },
	}, &ruRPClient{}, wf, &ruTxRunner{pub: &ruPublisher{}})
	require.NoError(t, svc.RemoveUser(callerCtx("tenant_admin"), uuid.New(), uuid.New(), uuid.New()))
	assert.Equal(t, 1, wf.impact)
}

func TestRemoveUser_OwnerMayRemoveAdmin(t *testing.T) {
	svc := buildRemoveUserSvc(lifecycleMembers(), lifecycleRoleRepo([]domain.TenantRoleCode{domain.RoleTenantAdmin}), &ruDeptMemRepo{
		softDeleteAllForUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.DeptMembership, error) { return nil, nil },
	}, &ruRPClient{}, &countingWorkflow{}, &ruTxRunner{pub: &ruPublisher{}})
	require.NoError(t, svc.RemoveUser(callerCtx("tenant_owner"), uuid.New(), uuid.New(), uuid.New()))
}

func TestSetStatus_AdminCannotSuspendPrivilegedMember(t *testing.T) {
	for _, role := range []domain.TenantRoleCode{domain.RoleTenantAdmin, domain.RoleTenantOwner} {
		setCalls := 0
		m := &fakeMembershipRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
			setCalls++
			return &domain.TenantMembership{}, nil
		}}
		svc := service.NewMembershipService(m, lifecycleRoleRepo([]domain.TenantRoleCode{role}), nil, &port.TenantRepositoryNoop{}, nil, nil, &fakeRPClient{}, nil, &passthroughTxRunner{}, nil)
		_, err := svc.SetStatus(callerCtx("tenant_admin"), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
		require.ErrorIs(t, err, domain.ErrInsufficientRole, role)
		assert.Zero(t, setCalls, role)
	}
}

func TestSetStatus_AdminMaySuspendNonPrivilegedMember(t *testing.T) {
	m := &fakeMembershipRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{Status: domain.MembershipSuspended}, nil
	}}
	svc := service.NewMembershipService(m, lifecycleRoleRepo([]domain.TenantRoleCode{domain.RoleTenderAdmin}), nil, &port.TenantRepositoryNoop{}, nil, nil, &fakeRPClient{}, nil, &passthroughTxRunner{}, nil)
	_, err := svc.SetStatus(callerCtx("tenant_admin"), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	require.NoError(t, err)
}

func TestSetStatus_OwnerMaySuspendAdmin(t *testing.T) {
	m := &fakeMembershipRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{Status: domain.MembershipSuspended}, nil
	}}
	svc := service.NewMembershipService(m, lifecycleRoleRepo([]domain.TenantRoleCode{domain.RoleTenantAdmin}), nil, &port.TenantRepositoryNoop{}, nil, nil, &fakeRPClient{}, nil, &passthroughTxRunner{}, nil)
	_, err := svc.SetStatus(callerCtx("tenant_owner"), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	require.NoError(t, err)
}

// ── Review follow-ups (rev 2.58): reactivate, P-26, P-34, P-28 lock, DST ──

type lockCountingTenants struct {
	port.TenantRepositoryNoop
	locks int
}

func (l *lockCountingTenants) LockByID(context.Context, uuid.UUID) error {
	l.locks++
	return nil
}

func TestSetStatus_AdminCannotReactivatePrivilegedMember(t *testing.T) {
	setCalls := 0
	m := &fakeMembershipRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
		setCalls++
		return &domain.TenantMembership{Status: domain.MembershipActive}, nil
	}}
	tenants := &lockCountingTenants{}
	svc := service.NewMembershipService(m, lifecycleRoleRepo([]domain.TenantRoleCode{domain.RoleTenantAdmin}), nil, tenants, nil, nil, &fakeRPClient{}, nil, &passthroughTxRunner{}, nil)
	_, err := svc.SetStatus(callerCtx("tenant_admin"), uuid.New(), uuid.New(), domain.MembershipActive, 1)
	require.ErrorIs(t, err, domain.ErrInsufficientRole)
	assert.Zero(t, setCalls)
	assert.Equal(t, 1, tenants.locks, "checked under the TM-13 lock")

	_, err = svc.SetStatus(callerCtx("tenant_owner"), uuid.New(), uuid.New(), domain.MembershipActive, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, setCalls)
}

func TestSetStatus_AdminReactivatesNonPrivilegedUnderLock(t *testing.T) {
	m := &fakeMembershipRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{Status: domain.MembershipActive}, nil
	}}
	tenants := &lockCountingTenants{}
	svc := service.NewMembershipService(m, lifecycleRoleRepo([]domain.TenantRoleCode{domain.RoleTenderAdmin}), nil, tenants, nil, nil, &fakeRPClient{}, nil, &passthroughTxRunner{}, nil)
	_, err := svc.SetStatus(callerCtx("tenant_admin"), uuid.New(), uuid.New(), domain.MembershipActive, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, tenants.locks)
}

// A role granted between the pre-check and the write is seen by the re-read
// under the lock: the first ListByUser returns no admin role, the second
// (inside the tx) does.
func TestSetStatus_SuspendRecheckUnderLockSeesConcurrentAdminGrant(t *testing.T) {
	calls := 0
	roles := &ruRoleRepo{listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
		calls++
		if calls == 1 {
			return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}}, nil
		}
		return []domain.TenantRole{{RoleCode: domain.RoleTenderAdmin}, {RoleCode: domain.RoleTenantAdmin}}, nil
	}}
	setCalls := 0
	m := &fakeMembershipRepo{setStatusFn: func(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
		setCalls++
		return &domain.TenantMembership{}, nil
	}}
	svc := service.NewMembershipService(m, roles, nil, &lockCountingTenants{}, nil, nil, &fakeRPClient{}, nil, &passthroughTxRunner{}, nil)
	_, err := svc.SetStatus(callerCtx("tenant_admin"), uuid.New(), uuid.New(), domain.MembershipSuspended, 1)
	require.ErrorIs(t, err, domain.ErrInsufficientRole)
	assert.Zero(t, setCalls)
}

func TestReconcileRoles_AdminRoleChangeTakesTenantLock(t *testing.T) {
	mem := &domain.TenantMembership{ID: uuid.New(), Status: domain.MembershipActive}
	m := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return mem, nil
	}}
	tenants := &lockCountingTenants{}
	svc := service.NewMembershipService(m, &extRoleRepo{}, nil, tenants, nil, nil, &fakeRPClient{}, nil, &rrTxRunner{pub: &rrPub{}}, nil)

	_, _, err := svc.ReconcileRoles(callerCtx("tenant_owner"), uuid.New(), uuid.New(), []domain.TenantRoleCode{domain.RoleTenantAdmin}, uuid.New())
	require.NoError(t, err)
	assert.Equal(t, 1, tenants.locks, "granting tenant_admin serialises with P-7's re-check")

	_, _, err = svc.ReconcileRoles(callerCtx("tenant_owner"), uuid.New(), uuid.New(), []domain.TenantRoleCode{domain.RoleTenderAdmin}, uuid.New())
	require.NoError(t, err)
	assert.Equal(t, 1, tenants.locks, "a tender_admin-only change takes no lock")
}

func TestResetUserMFA_AdminCannotResetPrivilegedMember(t *testing.T) {
	m := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{Status: domain.MembershipActive, RecordVersion: 1}, nil
	}}
	rp := &fakeRPClient{}
	svc := service.NewMembershipService(m, lifecycleRoleRepo([]domain.TenantRoleCode{domain.RoleTenantOwner}), nil, nil, nil, nil, rp, nil, &ruTxRunner{pub: &ruPublisher{}}, nil)
	err := svc.ResetUserMFA(callerCtx("tenant_admin"), uuid.New(), uuid.New(), uuid.New())
	require.ErrorIs(t, err, domain.ErrInsufficientRole)
	assert.False(t, rp.resetMFACalled)

	require.NoError(t, svc.ResetUserMFA(callerCtx("tenant_owner"), uuid.New(), uuid.New(), uuid.New()))
	assert.True(t, rp.resetMFACalled)
}

func TestResetUserMFA_AdminMayResetNonPrivilegedMember(t *testing.T) {
	m := &fakeMembershipRepo{findByUserIDFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{Status: domain.MembershipActive, RecordVersion: 1}, nil
	}}
	rp := &fakeRPClient{}
	svc := service.NewMembershipService(m, lifecycleRoleRepo(nil), nil, nil, nil, nil, rp, nil, &ruTxRunner{pub: &ruPublisher{}}, nil)
	require.NoError(t, svc.ResetUserMFA(callerCtx("tenant_admin"), uuid.New(), uuid.New(), uuid.New()))
	assert.True(t, rp.resetMFACalled)
}

func TestRemovalResolution_AdminCannotResolveForPrivilegedMember(t *testing.T) {
	wf := &countingWorkflow{}
	svc := buildRemoveUserSvc(lifecycleMembers(), lifecycleRoleRepo([]domain.TenantRoleCode{domain.RoleTenantAdmin}), nil, nil, wf, &ruTxRunner{})
	err := svc.RemovalResolution(callerCtx("tenant_admin"), uuid.New(), uuid.New(), service.RemovalStopWorkflows, nil, uuid.New())
	require.ErrorIs(t, err, domain.ErrInsufficientRole)
	assert.Zero(t, wf.cancel)

	require.NoError(t, svc.RemovalResolution(callerCtx("tenant_owner"), uuid.New(), uuid.New(), service.RemovalStopWorkflows, nil, uuid.New()))
	assert.Equal(t, 1, wf.cancel)
}

func TestRemovalResolution_AdminMayResolveForNonPrivilegedMember(t *testing.T) {
	wf := &countingWorkflow{}
	svc := buildRemoveUserSvc(lifecycleMembers(), lifecycleRoleRepo([]domain.TenantRoleCode{domain.RoleTenderAdmin}), nil, nil, wf, &ruTxRunner{})
	require.NoError(t, svc.RemovalResolution(callerCtx("tenant_admin"), uuid.New(), uuid.New(), service.RemovalStopWorkflows, nil, uuid.New()))
	assert.Equal(t, 1, wf.cancel)
}

// read_only_until is whole UTC days after cancelled_at, whatever zone the
// stored time carries, so it matches I-16's absolute-instant comparison.
func TestTenant_ReadOnlyUntil_UTCAcrossDST(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	require.NoError(t, err)
	// 2026-10-24 12:00 Berlin (CEST, +02:00); DST ends 2026-10-25.
	cancelled := time.Date(2026, 10, 24, 12, 0, 0, 0, berlin)
	tn := &domain.Tenant{Status: domain.StatusCancelled, CancelledAt: &cancelled}
	got := tn.ReadOnlyUntil(30)
	require.NotNil(t, got)
	assert.Equal(t, time.UTC, got.Location())
	assert.Equal(t, 30*24*time.Hour, got.Sub(cancelled), "exactly 30×24h, no DST hour")
}
