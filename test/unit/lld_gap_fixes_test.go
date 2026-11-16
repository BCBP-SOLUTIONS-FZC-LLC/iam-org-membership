package unit_test

// Service-level tests for the code-vs-LLD fixes of 2026-11-11 (LLD rev 2.52):
// SEAT-5 overage re-evaluation (GAP-7 / GAP-18), I-3 member_already_exists
// (GAP-24), PI-6 inline Keycloak-user delete on revoke (GAP-17), BUG-5
// character count, GAP-21 inactive department skipped by I-10.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── SEAT-5: ReevaluateSeatOverage ───────────────────────────────────────

type lgSeatTenantRepo struct {
	port.TenantRepositoryNoop
	occ     port.SeatOccupancy
	lockErr error
	set     []*time.Time
	calls   int
}

func (r *lgSeatTenantRepo) LockSeatOccupancy(context.Context, uuid.UUID) (port.SeatOccupancy, error) {
	return r.occ, r.lockErr
}
func (r *lgSeatTenantRepo) SetOverageSince(_ context.Context, _ uuid.UUID, since *time.Time) error {
	r.calls++
	r.set = append(r.set, since)
	return nil
}

func seatCtx(pub *ruPublisher) context.Context {
	return port.WithEventPublisher(context.Background(), pub)
}

func TestReevaluateSeatOverage_OverCapStartsMarkerAndEmits(t *testing.T) {
	repo := &lgSeatTenantRepo{occ: port.SeatOccupancy{LicensedSeats: 2, Active: 3}}
	pub := &ruPublisher{}
	got, err := service.ReevaluateSeatOverage(seatCtx(pub), repo, uuid.New(), service.SeatOverageSource{Actor: "iam-system"})
	require.NoError(t, err)
	assert.Equal(t, service.SeatOverageStarted, got)
	require.Len(t, repo.set, 1)
	assert.NotNil(t, repo.set[0], "overage_since set")
	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTenantSeatOverageStarted, pub.events[0].Type)
}

func TestReevaluateSeatOverage_BackUnderCapClearsMarkerAndEmits(t *testing.T) {
	since := time.Now().Add(-time.Hour)
	repo := &lgSeatTenantRepo{occ: port.SeatOccupancy{LicensedSeats: 5, Active: 3, OverageSince: &since}}
	pub := &ruPublisher{}
	got, err := service.ReevaluateSeatOverage(seatCtx(pub), repo, uuid.New(), service.SeatOverageSource{Actor: "u"})
	require.NoError(t, err)
	assert.Equal(t, service.SeatOverageResolved, got)
	require.Len(t, repo.set, 1)
	assert.Nil(t, repo.set[0], "overage_since cleared")
	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventTenantSeatOverageResolved, pub.events[0].Type)
}

func TestReevaluateSeatOverage_NoTransition(t *testing.T) {
	since := time.Now()
	for name, occ := range map[string]port.SeatOccupancy{
		"still over cap":  {LicensedSeats: 2, Active: 3, OverageSince: &since},
		"still under cap": {LicensedSeats: 5, Active: 3},
		"exactly at cap":  {LicensedSeats: 3, Active: 2, Pending: 1},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &lgSeatTenantRepo{occ: occ}
			pub := &ruPublisher{}
			got, err := service.ReevaluateSeatOverage(seatCtx(pub), repo, uuid.New(), service.SeatOverageSource{})
			require.NoError(t, err)
			assert.Equal(t, service.SeatOverageUnchanged, got)
			assert.Zero(t, repo.calls)
			assert.Empty(t, pub.events)
		})
	}
}

func TestReevaluateSeatOverage_LockErrorPropagates(t *testing.T) {
	lockErr := errors.New("lock failed")
	_, err := service.ReevaluateSeatOverage(context.Background(), &lgSeatTenantRepo{lockErr: lockErr}, uuid.New(), service.SeatOverageSource{})
	assert.ErrorIs(t, err, lockErr)
}

func TestReevaluateSeatOverage_NilRepoIsNoop(t *testing.T) {
	got, err := service.ReevaluateSeatOverage(context.Background(), nil, uuid.New(), service.SeatOverageSource{})
	require.NoError(t, err)
	assert.Equal(t, service.SeatOverageUnchanged, got)
}

// ── GAP-24 / PI-10: I-3 plain add for an existing member → 409 ──────────

type lgExistingMemberRepo struct {
	arMembershipRepo
	status   domain.MembershipStatus
	inserted bool
}

func (r *lgExistingMemberRepo) FindByUserID(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
	return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: r.status}, nil
}
func (r *lgExistingMemberRepo) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	r.inserted = true
	return nil, errors.New("Insert must not be reached for an existing member")
}

func TestAddFromRegister_PlainAdd_ExistingMember_409(t *testing.T) {
	for _, st := range []domain.MembershipStatus{domain.MembershipActive, domain.MembershipSuspended} {
		t.Run(string(st), func(t *testing.T) {
			mem := &lgExistingMemberRepo{status: st}
			svc := buildAddFromRegisterSvc(&arInviteRepo{}, &arTenantRepo{}, mem, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})
			_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "a@example.com")
			assert.ErrorIs(t, err, domain.ErrMemberAlreadyExists)
			assert.False(t, mem.inserted)
		})
	}
}

// A removed member (status='left') is not found by FindByUserID, so the
// plain add re-adds the user through Insert (TM-1 reactivation, GAP-16).
func TestAddFromRegister_PlainAdd_RemovedMember_ReAdded(t *testing.T) {
	inserted := false
	mem := &arMembershipRepo{insertFn: func(_ context.Context, m *domain.TenantMembership) (*domain.TenantMembership, error) {
		inserted = true
		return &domain.TenantMembership{ID: uuid.New(), TenantID: m.TenantID, UserID: m.UserID, Status: domain.MembershipActive}, nil
	}}
	svc := buildAddFromRegisterSvc(&arInviteRepo{}, &arTenantRepo{}, mem, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{})
	got, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.New(), "a@example.com")
	require.NoError(t, err)
	assert.True(t, inserted)
	assert.Equal(t, domain.MembershipActive, got.Status)
}

// ── SEAT-5: the I-3 plain add re-evaluates the overage marker ──────────

// lgI3SeatTenantRepo reports the occupancy seen after the add (the helper
// reads it under the tenant row lock, inside the same tx).
type lgI3SeatTenantRepo struct {
	arTenantRepo
	occ port.SeatOccupancy
	set []*time.Time
}

func (r *lgI3SeatTenantRepo) LockSeatOccupancy(context.Context, uuid.UUID) (port.SeatOccupancy, error) {
	return r.occ, nil
}
func (r *lgI3SeatTenantRepo) SetOverageSince(_ context.Context, _ uuid.UUID, since *time.Time) error {
	r.set = append(r.set, since)
	return nil
}

func TestAddFromRegister_PlainAdd_OverCap_StartsOverage(t *testing.T) {
	// Tenant was at exactly its 2 seats; the plain add makes 3 active.
	tenants := &lgI3SeatTenantRepo{occ: port.SeatOccupancy{LicensedSeats: 2, Active: 3}}
	pub := &arPub{}
	svc := buildAddFromRegisterSvc(&arInviteRepo{}, tenants, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{pub: pub})

	got, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.Nil, "")
	require.NoError(t, err, "SEAT-5 never blocks the add")
	assert.Equal(t, domain.MembershipActive, got.Status)
	require.Len(t, tenants.set, 1)
	assert.NotNil(t, tenants.set[0], "overage_since stamped")
	var started int
	for _, e := range pub.events {
		if e.Type == domain.EventTenantSeatOverageStarted {
			started++
		}
	}
	assert.Equal(t, 1, started)
}

func TestAddFromRegister_PlainAdd_AtCap_NoOverage(t *testing.T) {
	// 1 seat used of 2 before the add → exactly at cap after it: no marker.
	tenants := &lgI3SeatTenantRepo{occ: port.SeatOccupancy{LicensedSeats: 2, Active: 2}}
	pub := &arPub{}
	svc := buildAddFromRegisterSvc(&arInviteRepo{}, tenants, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, nil, &arTxRunner{pub: pub})

	_, err := svc.AddFromRegister(context.Background(), uuid.New(), uuid.New(), uuid.Nil, "")
	require.NoError(t, err)
	assert.Empty(t, tenants.set)
	assert.Empty(t, pub.events)
}

// ── GAP-17 / PI-6: revoke deletes the Keycloak user inline (best-effort) ──

type lgRevokeInviteRepo struct {
	arInviteRepo
	kcUser  *uuid.UUID
	cleared []uuid.UUID
	marked  bool
}

func (r *lgRevokeInviteRepo) SetStatus(_ context.Context, tid, id uuid.UUID, st domain.InvitationStatus, v int64) (*domain.PendingInvitation, error) {
	return &domain.PendingInvitation{ID: id, TenantID: tid, Status: st, RecordVersion: v + 1, KeycloakUserID: r.kcUser}, nil
}
func (r *lgRevokeInviteRepo) SetKCCleanupPending(context.Context, uuid.UUID, uuid.UUID, bool, int64) error {
	r.marked = true
	return nil
}
func (r *lgRevokeInviteRepo) ClearKCCleanupPendingByID(_ context.Context, id uuid.UUID) error {
	r.cleared = append(r.cleared, id)
	return nil
}

func revokeSvc(inv port.InvitationRepository, rp port.RealmProvisionerClient) *service.InvitationService {
	return service.NewInvitationService(inv, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, &arTenantRepo{}, rp, nil, &arTxRunner{}, nil, 7).
		WithReinviteCooldown(time.Hour)
}

func TestRevoke_InlineDeleteUser_SuccessClearsMarker(t *testing.T) {
	kc := uuid.New()
	inv := &lgRevokeInviteRepo{kcUser: &kc}
	rp := &invRPClient{}
	id := uuid.New()
	_, err := revokeSvc(inv, rp).Revoke(context.Background(), uuid.New(), id, 1)
	require.NoError(t, err)
	assert.True(t, inv.marked, "kc_cleanup_pending is still set first (PI-9 durable marker)")
	assert.Equal(t, 1, rp.deleteCalls, "PI-6: one inline DeleteUser")
	assert.Equal(t, []uuid.UUID{id}, inv.cleared, "marker cleared after a successful delete")
}

func TestRevoke_InlineDeleteUser_FailureKeepsMarkerAndSucceeds(t *testing.T) {
	kc := uuid.New()
	inv := &lgRevokeInviteRepo{kcUser: &kc}
	rp := &invRPClient{deleteFn: func(context.Context, uuid.UUID, uuid.UUID) error { return errors.New("rp down") }}
	_, err := revokeSvc(inv, rp).Revoke(context.Background(), uuid.New(), uuid.New(), 1)
	require.NoError(t, err, "a failed inline delete never fails the revoke")
	assert.Equal(t, 1, rp.deleteCalls)
	assert.Empty(t, inv.cleared, "marker kept for invitation-kc-cleanup")
}

func TestRevoke_NoKeycloakUser_NoInlineDelete(t *testing.T) {
	inv := &lgRevokeInviteRepo{}
	rp := &invRPClient{}
	_, err := revokeSvc(inv, rp).Revoke(context.Background(), uuid.New(), uuid.New(), 1)
	require.NoError(t, err)
	assert.Zero(t, rp.deleteCalls)
}

// ── BUG-5: the 512 limit counts characters, not bytes ───────────────────

// lgStopInviteRepo stops Invite at its first repository read, right after
// the input validation under test.
type lgStopInviteRepo struct{ depInviteRepo }

var errPastValidation = errors.New("reached the first repository read")

func (r *lgStopInviteRepo) FindPendingByEmail(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
	return nil, errPastValidation
}

func TestInvite_FullName300ArabicCharacters_PassesLengthCheck(t *testing.T) {
	name := strings.Repeat("ع", 300) // 300 characters, 600 bytes
	svc := service.NewInvitationService(&lgStopInviteRepo{}, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, &arTenantRepo{}, &invRPClient{}, nil, &arTxRunner{}, nil, 7)
	_, err := svc.Invite(context.Background(), uuid.New(), service.InvitationInput{Email: "a@example.com", FullName: name}, uuid.New())
	assert.ErrorIs(t, err, errPastValidation, "300 characters (600 bytes) is within the 512-character limit")
}

func TestInvite_FullName513Characters_Rejected(t *testing.T) {
	svc := service.NewInvitationService(&lgStopInviteRepo{}, &arMembershipRepo{}, &arRoleRepo{}, &arDeptMemRepo{}, &arTenantRepo{}, &invRPClient{}, nil, &arTxRunner{}, nil, 7)
	_, err := svc.Invite(context.Background(), uuid.New(), service.InvitationInput{Email: "a@example.com", FullName: strings.Repeat("ع", 513)}, uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrValidation)
}
