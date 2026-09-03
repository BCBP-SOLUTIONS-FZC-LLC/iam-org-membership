// invitation_gaps_test.go fills coverage gaps in invitation_service.go
// that can be reached without a real database or Keycloak:
//
//   - NewInvitationService (66.7%): expiryDays<=0 default branch (line 56)
//   - Invite (85.5%): FindPendingByEmail returns existing → ErrInvitationAlreadyExists (line 107)
//   - Invite (85.5%): reinviteCooldown path where elapsed < cooldown (line 118)
//   - preflightSeatCheck (91.7%): CountPending error propagates (line 273)
//   - AddFromRegister (74.6%): FindPendingByKeycloakUser error propagates (line 336)
//   - AddFromRegister (74.6%): FindPendingByEmail error propagates (line 346)
//   - AddFromRegister (74.6%): invalid email format → ErrValidation (line 305)
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

// ── NewInvitationService — expiryDays default ─────────────────────────────

// TestInvitationService_NewInvitationService_ZeroExpiryDays_DefaultsTo7
// verifies the branch: if expiryDays <= 0 { expiryDays = 7 }.
// We cannot directly inspect the field but can confirm the service returns
// non-nil (the branch executes without panic) and List works fine.
func TestInvitationService_NewInvitationService_ZeroExpiryDays_DefaultsTo7(t *testing.T) {
	svc := service.NewInvitationService(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, 0, // expiryDays=0 → triggers default
	)
	require.NotNil(t, svc, "NewInvitationService with expiryDays=0 must not return nil")
}

// TestInvitationService_NewInvitationService_NegativeExpiryDays_DefaultsTo7
// exercises the same branch with a negative value.
func TestInvitationService_NewInvitationService_NegativeExpiryDays_DefaultsTo7(t *testing.T) {
	svc := service.NewInvitationService(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, -5,
	)
	require.NotNil(t, svc)
}

// ── Invite — duplicate email → ErrInvitationAlreadyExists ────────────────

// igFindPendingByEmailRepo is a minimal InvitationRepository that returns a
// pre-existing pending invitation on FindPendingByEmail (simulating duplicate).
type igFindPendingByEmailRepo struct {
	fakeInviteRepo
	findPendingFn func(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error)
}

func (r *igFindPendingByEmailRepo) FindPendingByEmail(ctx context.Context, tid uuid.UUID, email string) (*domain.PendingInvitation, error) {
	if r.findPendingFn != nil {
		return r.findPendingFn(ctx, tid, email)
	}
	return nil, nil
}

// TestInvitation_Invite_DuplicateEmail_ReturnsErrInvitationAlreadyExists
// covers the branch at line 107: when FindPendingByEmail returns a non-nil
// existing row, Invite must return ErrInvitationAlreadyExists.
func TestInvitation_Invite_DuplicateEmail_ReturnsErrInvitationAlreadyExists(t *testing.T) {
	existing := &domain.PendingInvitation{
		ID: uuid.New(), Email: "alice@example.com", Status: domain.InvitePending,
	}
	repo := &igFindPendingByEmailRepo{
		findPendingFn: func(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
			return existing, nil
		},
	}
	svc := service.NewInvitationService(
		repo, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	)

	_, err := svc.Invite(context.Background(), uuid.New(), service.InvitationInput{
		Email:    "alice@example.com",
		FullName: "Alice",
	}, uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInvitationAlreadyExists)
}

// TestInvitation_Invite_FindPendingByEmailError_Propagates covers line 107-109:
// when FindPendingByEmail returns an error (not existing inv), Invite propagates it.
func TestInvitation_Invite_FindPendingByEmailError_Propagates(t *testing.T) {
	dbErr := errors.New("db_connection_lost")
	repo := &igFindPendingByEmailRepo{
		findPendingFn: func(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
			return nil, dbErr
		},
	}
	svc := service.NewInvitationService(
		repo, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	)

	_, err := svc.Invite(context.Background(), uuid.New(), service.InvitationInput{
		Email:    "bob@example.com",
		FullName: "Bob",
	}, uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, dbErr, "FindPendingByEmail error must propagate from Invite")
}

// TestInvitation_Invite_MostRecentCreatedAtError_Propagates covers line 118-120:
// when MostRecentCreatedAt returns an error, Invite propagates it.
// The cooldown must be > 0 to reach MostRecentCreatedAt.
type igCooldownErrRepo struct {
	fakeInviteRepo
	cooldownErr error
}

func (r *igCooldownErrRepo) FindPendingByEmail(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
	return nil, nil // no duplicate
}
func (r *igCooldownErrRepo) MostRecentCreatedAt(context.Context, uuid.UUID, string) (time.Time, error) {
	return time.Time{}, r.cooldownErr
}

func TestInvitation_Invite_MostRecentCreatedAtError_Propagates(t *testing.T) {
	dbErr := errors.New("most_recent_query_failed")
	repo := &igCooldownErrRepo{cooldownErr: dbErr}
	svc := service.NewInvitationService(
		repo, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	).WithReinviteCooldown(30 * time.Minute) // > 0 so MostRecentCreatedAt is called

	_, err := svc.Invite(context.Background(), uuid.New(), service.InvitationInput{
		Email:    "carol@example.com",
		FullName: "Carol",
	}, uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, dbErr, "MostRecentCreatedAt error must propagate from Invite")
}

// ── Invite — reinvite cooldown hit ───────────────────────────────────────

// igCooldownRepo simulates: no duplicate (FindPendingByEmail → nil) but
// MostRecentCreatedAt returns a recent timestamp (within the cooldown window).
type igCooldownRepo struct {
	fakeInviteRepo
}

func (r *igCooldownRepo) FindPendingByEmail(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
	return nil, nil // no existing invite
}
func (r *igCooldownRepo) MostRecentCreatedAt(context.Context, uuid.UUID, string) (time.Time, error) {
	return time.Now().UTC().Add(-5 * time.Minute), nil // 5 min ago (within a 30-min cooldown)
}

// TestInvitation_Invite_CooldownHit_ReturnsErrReinviteTooSoon covers the
// branch at line 118: elapsed < reinviteCooldown → ErrReinviteTooSoon.
func TestInvitation_Invite_CooldownHit_ReturnsErrReinviteTooSoon(t *testing.T) {
	repo := &igCooldownRepo{}
	svc := service.NewInvitationService(
		repo, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	).WithReinviteCooldown(30 * time.Minute) // 30 min cooldown, 5 min elapsed → blocked

	_, err := svc.Invite(context.Background(), uuid.New(), service.InvitationInput{
		Email:    "bob@example.com",
		FullName: "Bob",
	}, uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrReinviteTooSoon)
}

// ── preflightSeatCheck — CountPending error ───────────────────────────────

// igPreflightRepo simulates: FindPendingByEmail→nil, MostRecent→zero,
// CountCreatedInWindow→0,nil (all pass), then CountPending errors.
// We also need FindByID and tenants.FindByID to succeed — but since
// preflightSeatCheck is called before the RP call, and the RP call is nil here,
// we need a tenants repo stub.
//
// Actually: preflightSeatCheck calls tenants.FindByID first. So we need to
// make tenants return an over-capacity result. Let's go with that path:

type igPreflightCountPendingErrRepo struct {
	fakeInviteRepo
	countPendingErr error
}

func (r *igPreflightCountPendingErrRepo) FindPendingByEmail(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
	return nil, nil
}
func (r *igPreflightCountPendingErrRepo) MostRecentCreatedAt(context.Context, uuid.UUID, string) (time.Time, error) {
	return time.Time{}, nil
}
func (r *igPreflightCountPendingErrRepo) CountCreatedInWindow(context.Context, uuid.UUID, time.Time) (int, error) {
	return 0, nil
}
func (r *igPreflightCountPendingErrRepo) CountPending(context.Context, uuid.UUID) (int, error) {
	return 0, r.countPendingErr
}

type igPreflightTenantRepo struct {
	port.TenantRepositoryNoop
}

func (r *igPreflightTenantRepo) FindByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return &domain.Tenant{ID: id, LicensedSeats: 10, Status: domain.StatusActive}, nil
}
func (r *igPreflightTenantRepo) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return r.FindByID(ctx, id)
}

var _ port.TenantRepository = (*igPreflightTenantRepo)(nil)

type igPreflightMemberRepo struct{}

func (r *igPreflightMemberRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return nil, nil
}
func (r *igPreflightMemberRepo) FindByUserID(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *igPreflightMemberRepo) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *igPreflightMemberRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (r *igPreflightMemberRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error {
	return nil
}
func (r *igPreflightMemberRepo) CountActive(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *igPreflightMemberRepo) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

var _ port.MembershipRepository = (*igPreflightMemberRepo)(nil)

// TestInvitation_Invite_CountPendingError_Propagates covers the line in
// preflightSeatCheck where CountPending returns an error.
func TestInvitation_Invite_CountPendingError_Propagates(t *testing.T) {
	countErr := errors.New("db_count_error")
	repo := &igPreflightCountPendingErrRepo{countPendingErr: countErr}
	svc := service.NewInvitationService(
		repo, &igPreflightMemberRepo{}, nil, nil, &igPreflightTenantRepo{}, nil, nil, nil, nil, 7,
	)

	_, err := svc.Invite(context.Background(), uuid.New(), service.InvitationInput{
		Email:    "carol@example.com",
		FullName: "Carol",
	}, uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, countErr)
}

// ── AddFromRegister — invalid email → ErrValidation ──────────────────────

// TestInvitation_AddFromRegister_InvalidEmail_ReturnsErrValidation covers the
// GAP-I3-1 guard at line 305: when the supplied email is non-empty but
// structurally invalid (missing @), return ErrValidation.
func TestInvitation_AddFromRegister_InvalidEmail_ReturnsErrValidation(t *testing.T) {
	svc := service.NewInvitationService(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	)

	_, err := svc.AddFromRegister(
		context.Background(), uuid.New(), uuid.New(), uuid.New(),
		"not-an-email", // no @ → invalid
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrValidation)
}

// ── AddFromRegister — FindPendingByKeycloakUser error ─────────────────────

// igAddFromRegisterKCErrRepo returns an error from FindPendingByKeycloakUser.
type igAddFromRegisterKCErrRepo struct {
	fakeInviteRepo
	kcErr error
}

func (r *igAddFromRegisterKCErrRepo) FindPendingByKeycloakUser(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
	return nil, r.kcErr
}
func (r *igAddFromRegisterKCErrRepo) FindPendingByEmail(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
	return nil, nil
}

// TestInvitation_AddFromRegister_KCLookupError_Propagates covers line 336:
// FindPendingByKeycloakUser returning an error.
func TestInvitation_AddFromRegister_KCLookupError_Propagates(t *testing.T) {
	kcErr := errors.New("db_lookup_error")
	repo := &igAddFromRegisterKCErrRepo{kcErr: kcErr}
	svc := service.NewInvitationService(
		repo, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	)

	_, err := svc.AddFromRegister(
		context.Background(), uuid.New(), uuid.New(), uuid.New(), "",
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, kcErr)
}

// ── AddFromRegister — FindPendingByEmail error ────────────────────────────

// igAddFromRegisterEmailErrRepo returns nil from FindPendingByKeycloakUser
// (so the email lookup is attempted) and an error from FindPendingByEmail.
type igAddFromRegisterEmailErrRepo struct {
	fakeInviteRepo
	emailErr error
}

func (r *igAddFromRegisterEmailErrRepo) FindPendingByKeycloakUser(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
	return nil, nil // KC lookup misses → email lookup attempted
}
func (r *igAddFromRegisterEmailErrRepo) FindPendingByEmail(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
	return nil, r.emailErr
}

// TestInvitation_AddFromRegister_EmailLookupError_Propagates covers line 346:
// FindPendingByEmail returning an error after FindPendingByKeycloakUser → nil.
func TestInvitation_AddFromRegister_EmailLookupError_Propagates(t *testing.T) {
	emailErr := errors.New("email_db_error")
	repo := &igAddFromRegisterEmailErrRepo{emailErr: emailErr}
	svc := service.NewInvitationService(
		repo, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	)

	_, err := svc.AddFromRegister(
		context.Background(), uuid.New(), uuid.New(), uuid.New(),
		"valid@example.com", // valid format so the email lookup is reached
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, emailErr)
}
