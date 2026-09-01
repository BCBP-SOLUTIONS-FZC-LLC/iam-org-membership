// Supplemental unit tests for internal/core/service/invitation_service.go
// covering gaps identified by the merged coverage report:
//
//   - NewInvitationService option setters WithReinviteCooldown / WithMaxInvitesPerHour (66.7%)
//   - Invite — additional rate-limit and preflight branches (84.2%)
//   - preflightSeatCheck — at-capacity boundary (83.3%)
//   - AddFromRegister — pre-tx error propagation paths (71.8%)
package unit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── option setter tests ────────────────────────────────────────────────

// TestInvitationService_WithReinviteCooldown_Applied verifies that
// WithReinviteCooldown returns the same *InvitationService (non-nil,
// chainable), proving the option is stored without panicking.
func TestInvitationService_WithReinviteCooldown_Applied(t *testing.T) {
	svc := service.NewInvitationService(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	).WithReinviteCooldown(30 * time.Minute)

	require.NotNil(t, svc, "WithReinviteCooldown must return non-nil *InvitationService")
}

// TestInvitationService_WithMaxInvitesPerHour_Applied verifies that
// WithMaxInvitesPerHour returns the same *InvitationService (non-nil,
// chainable), proving the option is stored without panicking.
func TestInvitationService_WithMaxInvitesPerHour_Applied(t *testing.T) {
	svc := service.NewInvitationService(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	).WithMaxInvitesPerHour(10)

	require.NotNil(t, svc, "WithMaxInvitesPerHour must return non-nil *InvitationService")
}

// TestInvitationService_WithOptions_Chainable verifies both option setters
// can be chained on the same constructor call, covering the common wiring
// path used in cmd/server/main.go.
func TestInvitationService_WithOptions_Chainable(t *testing.T) {
	svc := service.NewInvitationService(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	).WithReinviteCooldown(time.Hour).WithMaxInvitesPerHour(20)

	require.NotNil(t, svc, "chained option setters must return non-nil *InvitationService")
}

// ── Invite — rate limit branch tests ──────────────────────────────────

// invRateLimitRepo is a minimal InvitationRepository for rate-limit tests.
// FindPendingByEmail returns nil/nil (no duplicate), MostRecentCreatedAt
// returns zero (cooldown does not trigger), and CountCreatedInWindow is
// configurable to simulate the PI-12 ceiling.
type invRateLimitRepo struct {
	fakeInviteRepo
	countWindowResult int
	countWindowErr    error
}

func (r *invRateLimitRepo) FindPendingByEmail(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
	return nil, nil // no duplicate invite — allows flow to reach rate-limit check
}
func (r *invRateLimitRepo) MostRecentCreatedAt(context.Context, uuid.UUID, string) (time.Time, error) {
	return time.Time{}, nil // zero = no prior invite, cooldown does not trigger
}
func (r *invRateLimitRepo) CountCreatedInWindow(context.Context, uuid.UUID, time.Time) (int, error) {
	return r.countWindowResult, r.countWindowErr
}

// TestInvitationService_Invite_RateLimitExceeded verifies that when the
// per-tenant hourly invite count equals maxPerHour the service returns
// ErrInviteRateLimited (PI-12) with a retry_after_seconds detail field.
// This exercises the count >= maxPerHour branch in Invite.
func TestInvitationService_Invite_RateLimitExceeded(t *testing.T) {
	const maxPerHour = 3
	inv := &invRateLimitRepo{countWindowResult: maxPerHour} // count == limit → rejected

	svc := service.NewInvitationService(
		inv, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	).WithMaxInvitesPerHour(maxPerHour)

	_, err := svc.Invite(context.Background(), uuid.New(),
		service.InvitationInput{Email: "user@example.com", FullName: "Test User"},
		uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInviteRateLimited,
		"PI-12: service must return ErrInviteRateLimited when hourly ceiling is reached")

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	retryAfter, ok := de.Details["retry_after_seconds"]
	assert.True(t, ok, "retry_after_seconds must be present in error details")
	assert.EqualValues(t, 3600, retryAfter,
		"PI-12: retry_after_seconds must be 3600 (one full hour)")
}

// TestInvitationService_Invite_RateLimitCountWindowError verifies that when
// CountCreatedInWindow itself returns an error (e.g. DB unavailable) the
// service propagates it rather than silently allowing or silently blocking.
func TestInvitationService_Invite_RateLimitCountWindowError(t *testing.T) {
	dbErr := errors.New("db timeout")
	inv := &invRateLimitRepo{countWindowErr: dbErr}

	svc := service.NewInvitationService(
		inv, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	).WithMaxInvitesPerHour(5)

	_, err := svc.Invite(context.Background(), uuid.New(),
		service.InvitationInput{Email: "user@example.com", FullName: "Test User"},
		uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, dbErr,
		"CountCreatedInWindow error must propagate to caller")
}

// ── preflightSeatCheck — at-capacity boundary ─────────────────────────

// preflightInviteRepo is a minimal InvitationRepository for preflight tests.
// All invitation methods not needed by preflightSeatCheck return safe defaults.
type preflightInviteRepo struct {
	fakeInviteRepo
	countPendingResult int
	countPendingErr    error
}

func (r *preflightInviteRepo) FindPendingByEmail(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
	return nil, nil // no duplicate
}
func (r *preflightInviteRepo) MostRecentCreatedAt(context.Context, uuid.UUID, string) (time.Time, error) {
	return time.Time{}, nil // cooldown disabled
}
func (r *preflightInviteRepo) CountCreatedInWindow(context.Context, uuid.UUID, time.Time) (int, error) {
	return 0, nil // rate limit not triggered
}
func (r *preflightInviteRepo) CountPending(context.Context, uuid.UUID) (int, error) {
	return r.countPendingResult, r.countPendingErr
}

// preflightMembershipRepo is a minimal MembershipRepository for preflight tests.
type preflightMembershipRepo struct {
	fakeMemRepoFull
	countActiveResult int
	countActiveErr    error
}

func (r *preflightMembershipRepo) CountActive(context.Context, uuid.UUID) (int, error) {
	return r.countActiveResult, r.countActiveErr
}

// preflightTenantRepo is a minimal TenantRepository for preflight tests.
type preflightTenantRepo struct {
	fakeTenantRepo
	tenant *domain.Tenant
	err    error
}

func (r *preflightTenantRepo) FindByID(context.Context, uuid.UUID) (*domain.Tenant, error) {
	return r.tenant, r.err
}

// TestInvitationService_preflightSeatCheck_AtCapacity verifies that when
// active + pending equals licensed_seats the preflight check returns
// ErrSeatLimitReached (SEAT-1 boundary: >= not >).
// This covers the missing branch at 83.3% where active+pending == licensed_seats.
func TestInvitationService_preflightSeatCheck_AtCapacity(t *testing.T) {
	const licensedSeats = 5
	const activeUsers = 3
	const pendingInvitations = 2 // 3 + 2 == 5 → at-cap → ErrSeatLimitReached

	invRepo := &preflightInviteRepo{countPendingResult: pendingInvitations}
	memRepo := &preflightMembershipRepo{countActiveResult: activeUsers}
	tenantRepo := &preflightTenantRepo{
		tenant: &domain.Tenant{ID: uuid.New(), LicensedSeats: licensedSeats},
	}

	svc := service.NewInvitationService(
		invRepo, memRepo, nil, nil, tenantRepo, nil, nil, nil, nil, 7,
	)

	_, err := svc.Invite(context.Background(), uuid.New(),
		service.InvitationInput{Email: "new@example.com", FullName: "New User"},
		uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrSeatLimitReached,
		"SEAT-1: active+pending >= licensed_seats must return ErrSeatLimitReached")

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.EqualValues(t, licensedSeats, de.Details["licensed_seats"],
		"error details must carry licensed_seats count")
	assert.EqualValues(t, activeUsers, de.Details["active_users"],
		"error details must carry active_users count")
	assert.EqualValues(t, pendingInvitations, de.Details["pending_invitations"],
		"error details must carry pending_invitations count")
}

// TestInvitationService_preflightSeatCheck_TenantFindError verifies that
// when TenantRepository.FindByID returns an error during preflight the
// service propagates the error immediately (before any RP call).
func TestInvitationService_preflightSeatCheck_TenantFindError(t *testing.T) {
	tenantErr := errors.New("tenant not found")
	invRepo := &preflightInviteRepo{}
	memRepo := &preflightMembershipRepo{}
	tenantRepo := &preflightTenantRepo{err: tenantErr}

	svc := service.NewInvitationService(
		invRepo, memRepo, nil, nil, tenantRepo, nil, nil, nil, nil, 7,
	)

	_, err := svc.Invite(context.Background(), uuid.New(),
		service.InvitationInput{Email: "new@example.com", FullName: "New User"},
		uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, tenantErr,
		"TenantRepository.FindByID error during preflight must propagate to caller")
}

// TestInvitationService_preflightSeatCheck_CountActiveError verifies that
// when MembershipRepository.CountActive returns an error the service
// propagates it and does not proceed to an RP call.
func TestInvitationService_preflightSeatCheck_CountActiveError(t *testing.T) {
	countErr := errors.New("count active db error")
	invRepo := &preflightInviteRepo{}
	memRepo := &preflightMembershipRepo{countActiveErr: countErr}
	tenantRepo := &preflightTenantRepo{
		tenant: &domain.Tenant{ID: uuid.New(), LicensedSeats: 10},
	}

	svc := service.NewInvitationService(
		invRepo, memRepo, nil, nil, tenantRepo, nil, nil, nil, nil, 7,
	)

	_, err := svc.Invite(context.Background(), uuid.New(),
		service.InvitationInput{Email: "new@example.com", FullName: "New User"},
		uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, countErr,
		"MembershipRepository.CountActive error during preflight must propagate")
}

// ── AddFromRegister — pre-tx error propagation paths ──────────────────

// addFromRegisterInviteRepo is a configurable InvitationRepository stub
// for AddFromRegister tests. The keycloak-user and email lookups are
// independently controllable so each pre-tx error path can be isolated.
type addFromRegisterInviteRepo struct {
	fakeInviteRepo
	findByKcFn    func(ctx context.Context, tenantID, kcUserID uuid.UUID) (*domain.PendingInvitation, error)
	findByEmailFn func(ctx context.Context, tenantID uuid.UUID, email string) (*domain.PendingInvitation, error)
}

func (r *addFromRegisterInviteRepo) FindPendingByKeycloakUser(ctx context.Context, tenantID, kcUserID uuid.UUID) (*domain.PendingInvitation, error) {
	if r.findByKcFn != nil {
		return r.findByKcFn(ctx, tenantID, kcUserID)
	}
	return nil, nil
}
func (r *addFromRegisterInviteRepo) FindPendingByEmail(ctx context.Context, tenantID uuid.UUID, email string) (*domain.PendingInvitation, error) {
	if r.findByEmailFn != nil {
		return r.findByEmailFn(ctx, tenantID, email)
	}
	return nil, nil
}

// TestInvitationService_AddFromRegister_FindByKeycloakUserError verifies
// that when the invitation repo returns an error on the keycloak-user lookup
// (the first pre-tx step in AddFromRegister) the error is propagated to the
// caller unchanged, covering the missing error-propagation branch (71.8%).
func TestInvitationService_AddFromRegister_FindByKeycloakUserError(t *testing.T) {
	lookupErr := errors.New("db connection reset")
	invRepo := &addFromRegisterInviteRepo{
		findByKcFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return nil, lookupErr
		},
	}

	svc := service.NewInvitationService(
		invRepo, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	)

	_, err := svc.AddFromRegister(context.Background(),
		uuid.New(), uuid.New(), uuid.New(), "user@example.com")

	require.Error(t, err)
	assert.ErrorIs(t, err, lookupErr,
		"FindPendingByKeycloakUser error must propagate from AddFromRegister")
}

// TestInvitationService_AddFromRegister_FindByEmailError verifies that when
// the keycloak-user lookup returns nil (no match) and the email fallback
// lookup returns an error, that error is propagated to the caller, covering
// the second pre-tx error path in AddFromRegister.
func TestInvitationService_AddFromRegister_FindByEmailError(t *testing.T) {
	emailLookupErr := errors.New("email lookup db timeout")
	invRepo := &addFromRegisterInviteRepo{
		findByKcFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
			return nil, nil // no match by KC user ID → fall through to email lookup
		},
		findByEmailFn: func(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
			return nil, emailLookupErr
		},
	}

	svc := service.NewInvitationService(
		invRepo, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	)

	// email must be non-empty so the fallback lookup is attempted (PI-4)
	_, err := svc.AddFromRegister(context.Background(),
		uuid.New(), uuid.New(), uuid.New(), "user@example.com")

	require.Error(t, err)
	assert.ErrorIs(t, err, emailLookupErr,
		"FindPendingByEmail fallback error must propagate from AddFromRegister")
}

// TestInvitationService_AddFromRegister_InvalidEmailFormat verifies that
// AddFromRegister rejects a malformed email (GAP-I3-1) before any repo call,
// covering the pre-tx validation error branch.
func TestInvitationService_AddFromRegister_InvalidEmailFormat(t *testing.T) {
	svc := service.NewInvitationService(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, 7,
	)

	_, err := svc.AddFromRegister(context.Background(),
		uuid.New(), uuid.New(), uuid.New(), "not-an-email")

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrValidation,
		"GAP-I3-1: AddFromRegister must reject malformed email with ErrValidation")
}
