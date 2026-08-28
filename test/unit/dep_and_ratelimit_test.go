// Unit tests for 6 infrastructure-dependent scenarios (I13-DEP-01, I4-DEP-01,
// I5-DEP-02, O7-EVT-03, I1-DEP-02, P6-429-02).
// I2-TX-02 is covered by TestProvisioning_SetRealmFields_UniqueConstraintViolation_Propagates
// in internal/core/service/provisioning_setrealm_test.go (whitebox — needs package-private stubs).
//
// These tests verify error propagation at the service layer when the database
// or downstream infrastructure is unavailable, without requiring a running
// DB or mock HTTP server.
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

// ── depInviteRepo ────────────────────────────────────────────────────────
// Minimal InvitationRepository stub for rate-limit and dependency tests.
// FindPendingByEmail → nil, nil (no existing invite so the flow continues).
// MostRecentCreatedAt → zero time (cooldown disabled / zero elapsed).
// CountCreatedInWindow → configurable (set countInWindowResult to trigger PI-12).

type depInviteRepo struct {
	countInWindowResult int
	countInWindowErr    error
}

func (r *depInviteRepo) List(context.Context, uuid.UUID) ([]domain.PendingInvitation, error) {
	return nil, errors.New("not implemented")
}
func (r *depInviteRepo) FindByID(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
	return nil, errors.New("not implemented")
}
func (r *depInviteRepo) FindPendingByEmail(context.Context, uuid.UUID, string) (*domain.PendingInvitation, error) {
	return nil, nil // no duplicate invite — lets flow proceed to rate-limit check
}
func (r *depInviteRepo) FindPendingByKeycloakUser(context.Context, uuid.UUID, uuid.UUID) (*domain.PendingInvitation, error) {
	return nil, errors.New("not implemented")
}
func (r *depInviteRepo) Insert(context.Context, *domain.PendingInvitation) (*domain.PendingInvitation, error) {
	return nil, errors.New("not implemented")
}
func (r *depInviteRepo) SetKeycloakUserID(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64) error {
	return errors.New("not implemented")
}
func (r *depInviteRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.InvitationStatus, int64) (*domain.PendingInvitation, error) {
	return nil, errors.New("not implemented")
}
func (r *depInviteRepo) SetKCCleanupPending(context.Context, uuid.UUID, uuid.UUID, bool, int64) error {
	return errors.New("not implemented")
}
func (r *depInviteRepo) CountPending(context.Context, uuid.UUID) (int, error) { return 0, nil }
func (r *depInviteRepo) ListExpiring(context.Context, time.Time, int) ([]domain.PendingInvitation, error) {
	return nil, errors.New("not implemented")
}
func (r *depInviteRepo) ListPendingKCCleanup(context.Context, int) ([]domain.PendingInvitation, error) {
	return nil, errors.New("not implemented")
}
func (r *depInviteRepo) MostRecentCreatedAt(context.Context, uuid.UUID, string) (time.Time, error) {
	return time.Time{}, nil // zero = no prior invite, cooldown does not trigger
}
func (r *depInviteRepo) CountCreatedInWindow(_ context.Context, _ uuid.UUID, _ time.Time) (int, error) {
	return r.countInWindowResult, r.countInWindowErr
}

var _ port.InvitationRepository = (*depInviteRepo)(nil)

// ── depPlanReader ─────────────────────────────────────────────────────────
// Returns a minimal Plan so TrialSignup can proceed past the pre-tx PlanByCode call.

type depPlanReader struct{ plan *domain.Plan }

func (r *depPlanReader) Plans(context.Context) ([]domain.Plan, error) {
	if r.plan != nil {
		return []domain.Plan{*r.plan}, nil
	}
	return nil, nil
}
func (r *depPlanReader) PlanByCode(_ context.Context, _ domain.TenantPlan) (*domain.Plan, error) {
	if r.plan != nil {
		return r.plan, nil
	}
	return &domain.Plan{Code: domain.PlanStarter, TrialDurationDays: 30}, nil
}

var _ port.PlanCatalogReader = (*depPlanReader)(nil)

// ── depDeptReader ─────────────────────────────────────────────────────────
// Returns the 5 system departments so TrialSignup can proceed past the
// pre-tx Departments call.

type depDeptReader struct{}

func (r *depDeptReader) Departments(context.Context) ([]domain.Department, error) {
	return []domain.Department{
		{ID: uuid.MustParse("de010001-0000-0000-0000-000000000001"), Code: "engineering", IsSystem: true, IsActive: true},
		{ID: uuid.MustParse("de010002-0000-0000-0000-000000000002"), Code: "design", IsSystem: true, IsActive: true},
		{ID: uuid.MustParse("de010003-0000-0000-0000-000000000003"), Code: "procurement", IsSystem: true, IsActive: true},
		{ID: uuid.MustParse("de010004-0000-0000-0000-000000000004"), Code: "finance", IsSystem: true, IsActive: true},
		{ID: uuid.MustParse("de010005-0000-0000-0000-000000000005"), Code: "legal", IsSystem: true, IsActive: true},
	}, nil
}
func (r *depDeptReader) DepartmentByID(_ context.Context, id uuid.UUID) (*domain.Department, error) {
	return nil, domain.NewError(domain.ErrDepartmentNotFound, "not found")
}

var _ port.DepartmentCatalogReader = (*depDeptReader)(nil)

// ════════════════════════════════════════════════════════════════════════
// P6-429-02 — per-tenant hourly invite ceiling hit → ErrInviteRateLimited
// ════════════════════════════════════════════════════════════════════════

// Test Case ID:      P6-429-02
// Scenario:          POST /tenants/:id/invitations when hourly invite ceiling is reached (PI-12)
//
//	→ 429 invite_rate_limited
//
// Coverage:          service.InvitationService.Invite → PI-12 rate-limit gate
func TestInvitation_Invite_RateLimitExceeded_Returns429(t *testing.T) {
	const maxPerHour = 5
	inv := &depInviteRepo{countInWindowResult: maxPerHour} // count == limit → rejected
	svc := service.NewInvitationService(inv, nil, nil, nil, nil, nil, nil, nil, nil, 7).
		WithMaxInvitesPerHour(maxPerHour)

	_, err := svc.Invite(context.Background(), uuid.New(), service.InvitationInput{
		Email:    "user@example.com",
		FullName: "Test User",
	}, uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInviteRateLimited,
		"PI-12: invite_rate_limited when hourly ceiling reached")
}

// ════════════════════════════════════════════════════════════════════════
// I4-DEP-01 — DB unavailable during PATCH membership status → error propagates
// ════════════════════════════════════════════════════════════════════════

// Test Case ID:      I4-DEP-01
// Scenario:          PATCH /internal/tenants/:id/members/:user_id when DB connection
//
//	lost (e.g. PgBouncer killed) → 503 db_unavailable
//
// Coverage:          service.ProvisioningService.SetMembershipStatus → propagates
//
//	MembershipRepository.SetStatus DB error
func TestProvisioning_SetMembershipStatus_DBUnavailable_Propagates(t *testing.T) {
	dbErr := domain.NewError(domain.ErrDBUnavailable, "pool exhausted")
	memRepo := &fakeMembershipRepo{
		setStatusFn: func(_ context.Context, _, _ uuid.UUID, _ domain.MembershipStatus, _ int64) (*domain.TenantMembership, error) {
			return nil, dbErr
		},
	}
	svc := service.NewProvisioningService(nil, nil, memRepo, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := svc.SetMembershipStatus(context.Background(),
		uuid.New(), uuid.New(), domain.MembershipActive, 1)

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrDBUnavailable,
		"I4-DEP-01: DB error must propagate from SetMembershipStatus")
}

// ════════════════════════════════════════════════════════════════════════
// I5-DEP-02 — DB unavailable during DELETE member → tx rollback
// ════════════════════════════════════════════════════════════════════════

// Test Case ID:      I5-DEP-02
// Scenario:          DELETE /internal/tenants/:id/members/:user_id when DB
//
//	connection is lost → 503 db_unavailable
//
// Coverage:          service.ProvisioningService.DeleteMember → propagates
//
//	TxRunner.RunInTx error (simulates DB unavailability on tx begin)
func TestProvisioning_DeleteMember_DBError_Propagates(t *testing.T) {
	dbErr := domain.NewError(domain.ErrDBUnavailable, "connection pool exhausted")
	txRunner := &passthroughTxRunner{runErr: dbErr}
	svc := service.NewProvisioningService(nil, nil, nil, nil, nil, nil, nil, nil, nil, txRunner, nil, nil)

	err := svc.DeleteMember(context.Background(), uuid.New(), uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrDBUnavailable,
		"I5-DEP-02: DB error in RunInTx must propagate from DeleteMember")
}

// ════════════════════════════════════════════════════════════════════════
// O7-EVT-03 — Outbox publish failure aborts tx → ReassignOwner returns error
// ════════════════════════════════════════════════════════════════════════

// Test Case ID:      O7-EVT-03
// Scenario:          POST /operator/tenants/:id/reassign-owner when the outbox
//
//	INSERT within the tx fails (DB kill mid-tx) → tx aborted → 503
//
// Coverage:          service.OperatorService.ReassignOwner → propagates
//
//	TxRunner.RunInTx error after pre-tx checks pass
func TestOperator_ReassignOwner_TxError_Propagates(t *testing.T) {
	txErr := domain.NewError(domain.ErrDBUnavailable, "outbox insert failed — tx aborted")
	tenantID := uuid.New()
	newOwnerID := uuid.New()

	tenantRepo := &fakeTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Status: domain.StatusActive}, nil
		},
	}
	memRepo := &fakeMemRepoFull{
		findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{
				ID:       uuid.New(),
				TenantID: tid,
				UserID:   uid,
				Status:   domain.MembershipActive,
			}, nil
		},
		countActiveFn: func(context.Context, uuid.UUID) (int, error) { return 1, nil },
		listFn: func(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
			return nil, errors.New("not used")
		},
	}

	svc := service.NewOperatorService(nil, tenantRepo, noOwnerRoleRepo{}, memRepo, nil,
		&passthroughTxRunner{runErr: txErr})

	_, err := svc.ReassignOwner(context.Background(), tenantID, newOwnerID, uuid.New())

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrDBUnavailable,
		"O7-EVT-03: tx error (outbox insert abort) must propagate from ReassignOwner")
}

// ════════════════════════════════════════════════════════════════════════
// I1-DEP-02 — DB unavailable during POST /internal/tenants (TrialSignup)
// ════════════════════════════════════════════════════════════════════════

// Test Case ID:      I1-DEP-02
// Scenario:          POST /internal/tenants when DB connection lost after
//
//	catalog pre-fetch succeeds → 503 db_unavailable
//
// Coverage:          service.ProvisioningService.TrialSignup → catalog calls
//
//	succeed, RunInTx returns DB error, propagated to caller
func TestProvisioning_TrialSignup_TxError_Propagates(t *testing.T) {
	dbErr := domain.NewError(domain.ErrDBUnavailable, "pool exhausted")
	txRunner := &passthroughTxRunner{runErr: dbErr}
	svc := service.NewProvisioningService(
		nil, nil, nil, nil, nil, nil, nil,
		&depDeptReader{}, &depPlanReader{},
		txRunner, nil, nil)

	_, _, err := svc.TrialSignup(context.Background(), service.TrialSignupInput{
		TenantID:      uuid.New(),
		Slug:          "test-tenant",
		Name:          "Test Tenant",
		Plan:          domain.PlanStarter,
		OwnerUserID:   uuid.New(),
		LicensedSeats: 10,
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrDBUnavailable,
		"I1-DEP-02: DB error in RunInTx must propagate from TrialSignup")
}

// ════════════════════════════════════════════════════════════════════════
// I13-DEP-01 — DB unavailable during assignee-override → actor role check
//
//	fails → 403 insufficient_role (code path intentionally maps DB errors
//	on the auth gate to ErrInsufficientRole for security — see LLD §5.4 I-13)
//
// ════════════════════════════════════════════════════════════════════════

// Test Case ID:      I13-DEP-01
// Scenario:          POST /internal/tenants/:id/tenders/:tid/assignee-override
//
//	when DB is unavailable — role check for actor fails →
//	returns ErrInsufficientRole (403), not 503, per security design
//
// Coverage:          service.MembershipService.ValidateAndEmitAssigneeOverride →
//
//	roles.ListByUser error → ErrInsufficientRole
func TestMembership_ValidateAndEmitAssigneeOverride_RoleCheckFails_ReturnsInsufficientRole(t *testing.T) {
	roleErr := errors.New("connection refused")
	roles := &fakeRoleRepo{
		listByUserFn: func(_ context.Context, _, _ uuid.UUID) ([]domain.TenantRole, error) {
			return nil, roleErr // DB unavailable → role lookup fails
		},
	}
	svc := service.NewMembershipService(
		nil, roles, nil, nil, nil, nil, nil, nil, nil, nil, 30)

	err := svc.ValidateAndEmitAssigneeOverride(
		context.Background(),
		uuid.New(), uuid.New(), uuid.New(), uuid.New(),
		domain.DeptPreparator,
		uuid.New(),
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrInsufficientRole,
		"I13-DEP-01: DB error on actor role check must map to ErrInsufficientRole (security design)")
}
