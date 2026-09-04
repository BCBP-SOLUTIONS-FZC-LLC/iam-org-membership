// Unit tests for internal/core/service/invitation_service.go Invite (P-6)
// and its preflightSeatCheck helper. These were previously exercised only
// via postgres integration tests (per invitation_service_test.go's header
// note) — but Invite's only non-trivial collaborators are TxRunner,
// TenantRepository, MembershipRepository, InvitationRepository, and
// RealmProvisionerClient, all plain interfaces fakeable the same way
// invitation_addfromregister_test.go already fakes AddFromRegister's
// collaborators. This file closes the remaining branch gaps: validation,
// duplicate-invite, PI-11/PI-12 pre-flight gates, preflightSeatCheck's own
// error/cap branches, the RP CreateInvitedUser failure, TOTP required-action
// selection, the transactional SEAT-1 recheck (all three read errors, the
// cap-reached orphan/kc_cleanup_pending commit, and the under-cap insert),
// the tx-failure compensating RP.DeleteUser (success + logged-failure), the
// lost-race seatLimitErr immediate RP.DeleteUser (success + logged-failure),
// and the happy-path cache invalidation.
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

// ── invTenantRepo — configurable FindByID + LicensedSeatsForUpdate ─────

type invTenantRepo struct {
	port.TenantRepositoryNoop
	findByIDFn             func(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
	licensedSeatsFn        func(ctx context.Context, id uuid.UUID) (int, error)
	clearOwnerlessSinceFn  func(ctx context.Context, id uuid.UUID) error
	markOwnerlessIfUnsetFn func(ctx context.Context, id uuid.UUID) (bool, error)
}

func (r *invTenantRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if r.findByIDFn != nil {
		return r.findByIDFn(ctx, id)
	}
	return &domain.Tenant{ID: id, LicensedSeats: 100}, nil
}

func (r *invTenantRepo) LicensedSeatsForUpdate(ctx context.Context, id uuid.UUID) (int, error) {
	if r.licensedSeatsFn != nil {
		return r.licensedSeatsFn(ctx, id)
	}
	return 100, nil
}

// clearOwnerlessSinceFn backs OperatorService.ReassignOwner's TM-12 clear —
// kept on this shared fake rather than a one-off type.
func (r *invTenantRepo) ClearOwnerlessSince(ctx context.Context, id uuid.UUID) error {
	if r.clearOwnerlessSinceFn != nil {
		return r.clearOwnerlessSinceFn(ctx, id)
	}
	return nil
}

// markOwnerlessIfUnsetFn backs ProvisioningService.DeleteMember's TM-12
// ownerless-escalation set — kept here rather than a one-off type.
func (r *invTenantRepo) MarkOwnerlessIfUnset(ctx context.Context, id uuid.UUID) (bool, error) {
	if r.markOwnerlessIfUnsetFn != nil {
		return r.markOwnerlessIfUnsetFn(ctx, id)
	}
	return false, nil
}

var _ port.TenantRepository = (*invTenantRepo)(nil)

// ── invInviteRepo — configurable Invite-path InvitationRepository ──────

type invInviteRepo struct {
	arInviteRepo
	insertFn        func(ctx context.Context, inv *domain.PendingInvitation) (*domain.PendingInvitation, error)
	mostRecentFn    func(ctx context.Context, tenantID uuid.UUID, email string) (time.Time, error)
	countInWindowFn func(ctx context.Context, tenantID uuid.UUID, since time.Time) (int, error)
	countPendingFn  func(ctx context.Context, tenantID uuid.UUID) (int, error)
}

func (r *invInviteRepo) Insert(ctx context.Context, inv *domain.PendingInvitation) (*domain.PendingInvitation, error) {
	if r.insertFn != nil {
		return r.insertFn(ctx, inv)
	}
	out := *inv
	out.ID = uuid.New()
	out.RecordVersion = 1
	return &out, nil
}

// CountPending overrides fakeInviteRepo's promoted default (which errors
// with "not used" unless configured) with a safe zero-value default —
// Invite calls this twice (preflightSeatCheck, then again inside the tx),
// so most Invite tests need it to just work unless a test cares about it.
func (r *invInviteRepo) CountPending(ctx context.Context, tenantID uuid.UUID) (int, error) {
	if r.countPendingFn != nil {
		return r.countPendingFn(ctx, tenantID)
	}
	return 0, nil
}

func (r *invInviteRepo) MostRecentCreatedAt(ctx context.Context, tenantID uuid.UUID, email string) (time.Time, error) {
	if r.mostRecentFn != nil {
		return r.mostRecentFn(ctx, tenantID, email)
	}
	return time.Time{}, nil
}

func (r *invInviteRepo) CountCreatedInWindow(ctx context.Context, tenantID uuid.UUID, since time.Time) (int, error) {
	if r.countInWindowFn != nil {
		return r.countInWindowFn(ctx, tenantID, since)
	}
	return 0, nil
}

var _ port.InvitationRepository = (*invInviteRepo)(nil)

// ── invRPClient — configurable RealmProvisionerClient ───────────────────

type invRPClient struct {
	createFn    func(ctx context.Context, req port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error)
	deleteFn    func(ctx context.Context, tenantID, kcUserID uuid.UUID) error
	deleteCalls int
	lastReq     port.CreateInvitedUserRequest
}

func (r *invRPClient) CreateInvitedUser(ctx context.Context, req port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	r.lastReq = req
	if r.createFn != nil {
		return r.createFn(ctx, req)
	}
	return &port.CreateInvitedUserResponse{KeycloakUserID: uuid.New()}, nil
}
func (r *invRPClient) DeleteUser(ctx context.Context, tenantID, kcUserID uuid.UUID) error {
	r.deleteCalls++
	if r.deleteFn != nil {
		return r.deleteFn(ctx, tenantID, kcUserID)
	}
	return nil
}
func (r *invRPClient) PatchRealmConfig(context.Context, uuid.UUID, port.RealmConfigPatch) error {
	return nil
}
func (r *invRPClient) RevokeUserSessions(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (r *invRPClient) ResetMFA(context.Context, uuid.UUID, uuid.UUID) error           { return nil }

var _ port.RealmProvisionerClient = (*invRPClient)(nil)

// buildInviteSvc wires every collaborator Invite touches, with sane
// zero-value defaults (no cooldown, no rate limit).
func buildInviteSvc(inv port.InvitationRepository, tenants port.TenantRepository, mem port.MembershipRepository,
	rp port.RealmProvisionerClient, cache port.Cache, tr port.TxRunner,
) *service.InvitationService {
	return service.NewInvitationService(inv, mem, nil, nil, tenants, rp, cache, tr, nil, 7)
}

func validInviteInput() service.InvitationInput {
	return service.InvitationInput{Email: "new@example.com", FullName: "New User"}
}

// Note: Invite's other validation branches (bad email format, empty
// full_name, non-elevated initial role) are already covered by
// invitation_validation_test.go — not duplicated here. The empty-email
// check is the one branch that file doesn't reach (its "invalid format"
// cases all use a non-empty string), so it's covered here instead.

func TestInvite_EmptyEmail_ValidationError(t *testing.T) {
	svc := buildInviteSvc(&invInviteRepo{}, &invTenantRepo{}, &arMembershipRepo{}, &invRPClient{}, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), service.InvitationInput{FullName: "X"}, uuid.New())
	assert.ErrorIs(t, err, domain.ErrValidation)
}

// ── duplicate-invite check ──────────────────────────────────────────────

func TestInvite_FindPendingByEmailErrorPropagates(t *testing.T) {
	findErr := errors.New("db down")
	inv := &invInviteRepo{arInviteRepo: arInviteRepo{findByEmailFn: func(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
		return nil, findErr
	}}}
	svc := buildInviteSvc(inv, &invTenantRepo{}, &arMembershipRepo{}, &invRPClient{}, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, findErr)
}

func TestInvite_ExistingPendingInvitation_AlreadyExistsError(t *testing.T) {
	inv := &invInviteRepo{arInviteRepo: arInviteRepo{findByEmailFn: func(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
		return &domain.PendingInvitation{ID: uuid.New(), Status: domain.InvitePending}, nil
	}}}
	svc := buildInviteSvc(inv, &invTenantRepo{}, &arMembershipRepo{}, &invRPClient{}, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrInvitationAlreadyExists)
}

// ── PI-11 reinvite cooldown ──────────────────────────────────────────────

func TestInvite_ReinviteCooldown_MostRecentErrorPropagates(t *testing.T) {
	mrErr := errors.New("db down")
	inv := &invInviteRepo{mostRecentFn: func(context.Context, uuid.UUID, string) (time.Time, error) {
		return time.Time{}, mrErr
	}}
	svc := buildInviteSvc(inv, &invTenantRepo{}, &arMembershipRepo{}, &invRPClient{}, nil, &arTxRunner{}).
		WithReinviteCooldown(time.Hour)
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, mrErr)
}

// Note: the cooldown-triggered 429 branch is already covered by
// TestInvite_ReinviteCooldown_Returns429 in invitation_scenarios_test.go.

func TestInvite_ReinviteCooldown_ElapsedAllowsInvite(t *testing.T) {
	inv := &invInviteRepo{mostRecentFn: func(context.Context, uuid.UUID, string) (time.Time, error) {
		return time.Now().UTC().Add(-2 * time.Hour), nil
	}}
	svc := buildInviteSvc(inv, &invTenantRepo{}, &arMembershipRepo{}, &invRPClient{}, nil, &arTxRunner{}).
		WithReinviteCooldown(time.Hour)
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	require.NoError(t, err)
}

// ── PI-12 hourly rate limit ───────────────────────────────────────────────

func TestInvite_RateLimit_CountErrorPropagates(t *testing.T) {
	countErr := errors.New("db down")
	inv := &invInviteRepo{countInWindowFn: func(context.Context, uuid.UUID, time.Time) (int, error) {
		return 0, countErr
	}}
	svc := buildInviteSvc(inv, &invTenantRepo{}, &arMembershipRepo{}, &invRPClient{}, nil, &arTxRunner{}).
		WithMaxInvitesPerHour(5)
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, countErr)
}

func TestInvite_RateLimit_Reached(t *testing.T) {
	inv := &invInviteRepo{countInWindowFn: func(context.Context, uuid.UUID, time.Time) (int, error) {
		return 5, nil
	}}
	svc := buildInviteSvc(inv, &invTenantRepo{}, &arMembershipRepo{}, &invRPClient{}, nil, &arTxRunner{}).
		WithMaxInvitesPerHour(5)
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrInviteRateLimited)
}

// ── preflightSeatCheck branches (advisory pre-tx check) ──────────────────

func TestInvite_Preflight_FindByIDErrorPropagates(t *testing.T) {
	findErr := errors.New("db down")
	tenants := &invTenantRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Tenant, error) {
		return nil, findErr
	}}
	svc := buildInviteSvc(&invInviteRepo{}, tenants, &arMembershipRepo{}, &invRPClient{}, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, findErr)
}

func TestInvite_Preflight_CountActiveErrorPropagates(t *testing.T) {
	countErr := errors.New("db down")
	mem := &arMembershipRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 0, countErr }}
	svc := buildInviteSvc(&invInviteRepo{}, &invTenantRepo{}, mem, &invRPClient{}, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, countErr)
}

func TestInvite_Preflight_CountPendingErrorPropagates(t *testing.T) {
	pendingErr := errors.New("db down")
	inv := &invInviteRepo{countPendingFn: func(context.Context, uuid.UUID) (int, error) { return 0, pendingErr }}
	svc := buildInviteSvc(inv, &invTenantRepo{}, &arMembershipRepo{}, &invRPClient{}, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, pendingErr)
}

func TestInvite_Preflight_CapReached_SeatLimitError(t *testing.T) {
	tenants := &invTenantRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
		return &domain.Tenant{ID: id, LicensedSeats: 5}, nil
	}}
	mem := &arMembershipRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 5, nil }}
	svc := buildInviteSvc(&invInviteRepo{}, tenants, mem, &invRPClient{}, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.ErrorIs(t, err, domain.ErrSeatLimitReached)
	assert.EqualValues(t, 5, de.Details["licensed_seats"])
}

// ── RP CreateInvitedUser failure ──────────────────────────────────────────

func TestInvite_RPCreateInvitedUserErrorPropagates(t *testing.T) {
	rp := &invRPClient{createFn: func(context.Context, port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
		return nil, errors.New("RP unreachable")
	}}
	svc := buildInviteSvc(&invInviteRepo{}, &invTenantRepo{}, &arMembershipRepo{}, rp, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrRealmProvisionerUnavailable)
}

// ── required-action TOTP selection (F5) ───────────────────────────────────

func TestInvite_RequiredActions_TOTPAddedForTenantAdminRole(t *testing.T) {
	rp := &invRPClient{}
	svc := buildInviteSvc(&invInviteRepo{}, &invTenantRepo{}, &arMembershipRepo{}, rp, nil, &arTxRunner{})
	in := validInviteInput()
	in.InitialTenantRoles = []domain.TenantRoleCode{domain.RoleTenantAdmin}
	_, err := svc.Invite(context.Background(), uuid.New(), in, uuid.New())
	require.NoError(t, err)
	assert.Contains(t, rp.lastReq.RequiredActions, port.RequiredActionConfigureTOTP)
}

func TestInvite_RequiredActions_TOTPAddedForApproverDeptMapping(t *testing.T) {
	rp := &invRPClient{}
	svc := buildInviteSvc(&invInviteRepo{}, &invTenantRepo{}, &arMembershipRepo{}, rp, nil, &arTxRunner{})
	in := validInviteInput()
	in.InitialDeptMappings = []domain.InvitationDeptMapping{{DepartmentID: uuid.New(), Level: domain.DeptApprover}}
	_, err := svc.Invite(context.Background(), uuid.New(), in, uuid.New())
	require.NoError(t, err)
	assert.Contains(t, rp.lastReq.RequiredActions, port.RequiredActionConfigureTOTP)
}

func TestInvite_RequiredActions_NoTOTPForPlainInvite(t *testing.T) {
	rp := &invRPClient{}
	svc := buildInviteSvc(&invInviteRepo{}, &invTenantRepo{}, &arMembershipRepo{}, rp, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	require.NoError(t, err)
	assert.NotContains(t, rp.lastReq.RequiredActions, port.RequiredActionConfigureTOTP)
}

// ── transactional SEAT-1 recheck: read-error branches ─────────────────────

func TestInvite_Tx_LicensedSeatsForUpdateErrorPropagates(t *testing.T) {
	seatErr := errors.New("db down")
	tenants := &invTenantRepo{licensedSeatsFn: func(context.Context, uuid.UUID) (int, error) { return 0, seatErr }}
	svc := buildInviteSvc(&invInviteRepo{}, tenants, &arMembershipRepo{}, &invRPClient{}, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, seatErr)
}

// Both preflightSeatCheck and the in-tx SEAT-1 recheck call
// memberships.CountActive / invites.CountPending — preflight runs first, so
// a statically-erroring fake would trip on the preflight line, never
// reaching the tx's own copy. These two tests use a call counter so the
// first (preflight) call succeeds and only the second (in-tx) call errors,
// exercising the tx block's own lines.

func TestInvite_Tx_CountActiveErrorPropagates(t *testing.T) {
	countErr := errors.New("db down")
	calls := 0
	mem := &arMembershipRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) {
		calls++
		if calls == 1 {
			return 1, nil // preflight: passes
		}
		return 0, countErr // in-tx recheck: fails
	}}
	svc := buildInviteSvc(&invInviteRepo{}, &invTenantRepo{}, mem, &invRPClient{}, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, countErr)
	assert.Equal(t, 2, calls, "must reach the in-tx CountActive call, not just preflight's")
}

func TestInvite_Tx_CountPendingErrorPropagates(t *testing.T) {
	pendingErr := errors.New("db down")
	calls := 0
	inv := &invInviteRepo{countPendingFn: func(context.Context, uuid.UUID) (int, error) {
		calls++
		if calls == 1 {
			return 0, nil // preflight: passes
		}
		return 0, pendingErr // in-tx recheck: fails
	}}
	svc := buildInviteSvc(inv, &invTenantRepo{}, &arMembershipRepo{}, &invRPClient{}, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, pendingErr)
	assert.Equal(t, 2, calls, "must reach the in-tx CountPending call, not just preflight's")
}

// ── transactional SEAT-1 recheck: cap reached → durable orphan row (PI-9) ─

func TestInvite_Tx_CapReached_OrphanInsertErrorPropagates(t *testing.T) {
	insertErr := errors.New("insert failed")
	tenants := &invTenantRepo{licensedSeatsFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil }}
	mem := &arMembershipRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil }}
	inv := &invInviteRepo{insertFn: func(context.Context, *domain.PendingInvitation) (*domain.PendingInvitation, error) {
		return nil, insertErr
	}}
	svc := buildInviteSvc(inv, tenants, mem, &invRPClient{}, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, insertErr)
}

func TestInvite_Tx_CapReached_SetKCCleanupPendingErrorPropagates(t *testing.T) {
	cleanupErr := errors.New("db down")
	tenants := &invTenantRepo{licensedSeatsFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil }}
	mem := &arMembershipRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil }}
	inv := &invInviteRepo{}
	inv.setKCCleanupPendingFn = func(context.Context, uuid.UUID, uuid.UUID, bool, int64) error {
		return cleanupErr
	}
	svc := buildInviteSvc(inv, tenants, mem, &invRPClient{}, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, cleanupErr)
}

func TestInvite_Tx_CapReached_ReturnsSeatLimitErrorAndDeletesKCUser(t *testing.T) {
	tenants := &invTenantRepo{licensedSeatsFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil }}
	mem := &arMembershipRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil }}
	inv := &invInviteRepo{}
	inv.setKCCleanupPendingFn = func(context.Context, uuid.UUID, uuid.UUID, bool, int64) error { return nil }
	rp := &invRPClient{}
	cache := &spyCache{}
	svc := buildInviteSvc(inv, tenants, mem, rp, cache, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.ErrorIs(t, err, domain.ErrSeatLimitReached)
	assert.Equal(t, 1, rp.deleteCalls, "lost SEAT-1 race must trigger an immediate best-effort RP.DeleteUser")
	assert.Empty(t, cache.deleteCalls, "no cache invalidation on a failed invite")
}

func TestInvite_Tx_CapReached_ImmediateDeleteUserFailureStillReturnsSeatLimitError(t *testing.T) {
	tenants := &invTenantRepo{licensedSeatsFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil }}
	mem := &arMembershipRepo{countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil }}
	inv := &invInviteRepo{}
	inv.setKCCleanupPendingFn = func(context.Context, uuid.UUID, uuid.UUID, bool, int64) error { return nil }
	rp := &invRPClient{deleteFn: func(context.Context, uuid.UUID, uuid.UUID) error {
		return errors.New("RP delete failed")
	}}
	svc := buildInviteSvc(inv, tenants, mem, rp, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrSeatLimitReached, "immediate-delete failure is only logged — the durable kc_cleanup_pending row is the correctness mechanism")
	assert.Equal(t, 1, rp.deleteCalls)
}

// ── transactional SEAT-1 recheck: under cap → insert succeeds ─────────────

func TestInvite_Tx_UnderCap_InsertErrorPropagates(t *testing.T) {
	insertErr := errors.New("uq violation")
	inv := &invInviteRepo{insertFn: func(context.Context, *domain.PendingInvitation) (*domain.PendingInvitation, error) {
		return nil, insertErr
	}}
	svc := buildInviteSvc(inv, &invTenantRepo{}, &arMembershipRepo{}, &invRPClient{}, nil, &arTxRunner{})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, insertErr)
}

// ── tx-runner-level failure → compensating RP.DeleteUser ──────────────────

func TestInvite_TxFails_CompensatingDeleteUserSucceeds(t *testing.T) {
	txErr := errors.New("tx aborted")
	rp := &invRPClient{}
	svc := buildInviteSvc(&invInviteRepo{}, &invTenantRepo{}, &arMembershipRepo{}, rp, nil, &passthroughTxRunner{runErr: txErr})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, txErr)
	assert.Equal(t, 1, rp.deleteCalls, "a tx failure must trigger a best-effort compensating RP.DeleteUser")
}

func TestInvite_TxFails_CompensatingDeleteUserAlsoFails_StillReturnsOriginalTxError(t *testing.T) {
	txErr := errors.New("tx aborted")
	rp := &invRPClient{deleteFn: func(context.Context, uuid.UUID, uuid.UUID) error {
		return errors.New("RP also unreachable")
	}}
	svc := buildInviteSvc(&invInviteRepo{}, &invTenantRepo{}, &arMembershipRepo{}, rp, nil, &passthroughTxRunner{runErr: txErr})
	_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
	assert.ErrorIs(t, err, txErr, "the compensating-delete failure is only logged, the original tx error is returned")
	assert.Equal(t, 1, rp.deleteCalls)
}

// ── happy path ──────────────────────────────────────────────────────────

func TestInvite_Success_InsertsAndInvalidatesSeatUsageCache(t *testing.T) {
	tenantID := uuid.New()
	cache := &spyCache{}
	svc := buildInviteSvc(&invInviteRepo{}, &invTenantRepo{}, &arMembershipRepo{}, &invRPClient{}, cache, &arTxRunner{})
	got, err := svc.Invite(context.Background(), tenantID, validInviteInput(), uuid.New())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "new@example.com", got.Email)
	assert.Contains(t, cache.deleteCalls, "om:seat_usage:"+tenantID.String())
}

func TestInvite_Success_NilCacheIsSafe(t *testing.T) {
	svc := buildInviteSvc(&invInviteRepo{}, &invTenantRepo{}, &arMembershipRepo{}, &invRPClient{}, nil, &arTxRunner{})
	assert.NotPanics(t, func() {
		_, err := svc.Invite(context.Background(), uuid.New(), validInviteInput(), uuid.New())
		require.NoError(t, err)
	})
}
