package unit_test

// Service-level tests for the code-vs-LLD fixes of 2026-11-11 (LLD rev 2.54):
// I-3 member_already_exists (GAP-24), PI-6 inline Keycloak-user delete on
// revoke (GAP-17), BUG-5 character count, GAP-21 inactive department skipped
// by I-10.

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
