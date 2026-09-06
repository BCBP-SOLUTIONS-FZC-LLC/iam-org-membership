// operator_handler_gaps_test.go — branch coverage for OperatorHandler.ReassignOwner
// paths that are not reachable via the nil-service (absorbPanic) pattern:
//
//  1. svc.ActiveRoleCodes returns an error after ReassignOwner succeeds
//     → HandleError path (lines 125-128 in operator_handler.go).
//  2. Full success: both ReassignOwner and ActiveRoleCodes succeed
//     → 200 with roles array in the response body.
//
// Both tests wire a real *service.OperatorService with controllable stub
// repositories, matching the pattern used in tenant_happy_test.go and
// membership_happy_test.go.
package http

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ── Local stubs for OperatorService (prefix "ogh") ────────────────────────────

// oghTenantRepo is a TenantRepository stub that embeds TenantRepositoryNoop
// so only the methods exercised by OperatorService need to be overridden.
type oghTenantRepo struct {
	port.TenantRepositoryNoop
	findByIDFn func(context.Context, uuid.UUID) (*domain.Tenant, error)
}

func (f *oghTenantRepo) FindByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if f.findByIDFn != nil {
		return f.findByIDFn(ctx, id)
	}
	return &domain.Tenant{ID: id, Status: domain.StatusActive}, nil
}

// FindByIDIncludingDeleted delegates to FindByID for test purposes.
func (f *oghTenantRepo) FindByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return f.FindByID(ctx, id)
}

// ClearOwnerlessSince is called by OperatorService.ReassignOwner and must succeed.
func (f *oghTenantRepo) ClearOwnerlessSince(_ context.Context, _ uuid.UUID) error { return nil }

var _ port.TenantRepository = (*oghTenantRepo)(nil)

// oghRoleRepo is a TenantRoleRepository stub for OperatorService tests.
type oghRoleRepo struct {
	grantFn      func(context.Context, *domain.TenantRole) (*domain.TenantRole, error)
	listByUserFn func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error)
}

func (f *oghRoleRepo) Grant(ctx context.Context, r *domain.TenantRole) (*domain.TenantRole, error) {
	if f.grantFn != nil {
		return f.grantFn(ctx, r)
	}
	r.ID = uuid.New()
	return r, nil
}

func (f *oghRoleRepo) ListByUser(ctx context.Context, tid, uid uuid.UUID) ([]domain.TenantRole, error) {
	if f.listByUserFn != nil {
		return f.listByUserFn(ctx, tid, uid)
	}
	return nil, nil
}

func (f *oghRoleRepo) ListByRole(context.Context, uuid.UUID, domain.TenantRoleCode) ([]domain.TenantRole, error) {
	return nil, nil
}
func (f *oghRoleRepo) CountActiveOwners(context.Context, uuid.UUID) (int, error) {
	return 0, nil
}
func (f *oghRoleRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, domain.TenantRoleCode) (*domain.TenantRole, error) {
	return nil, nil
}
func (f *oghRoleRepo) SoftDeleteAllForUser(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
	return nil, nil
}

var _ port.TenantRoleRepository = (*oghRoleRepo)(nil)

// oghMemRepo is a MembershipRepository stub for OperatorService tests.
// Only FindByUserID and ListActiveUserIDs are exercised by OperatorService.
type oghMemRepo struct {
	findByUserFn func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error)
}

func (f *oghMemRepo) FindByUserID(ctx context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
	if f.findByUserFn != nil {
		return f.findByUserFn(ctx, tid, uid)
	}
	return &domain.TenantMembership{
		ID: uuid.New(), TenantID: tid, UserID: uid,
		Status: domain.MembershipActive, RecordVersion: 1,
	}, nil
}

func (f *oghMemRepo) List(context.Context, uuid.UUID, *domain.MembershipListCursor, int) (*domain.MembershipListPage, error) {
	return &domain.MembershipListPage{}, nil
}
func (f *oghMemRepo) Insert(context.Context, *domain.TenantMembership) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *oghMemRepo) SetStatus(context.Context, uuid.UUID, uuid.UUID, domain.MembershipStatus, int64) (*domain.TenantMembership, error) {
	return nil, nil
}
func (f *oghMemRepo) SoftDelete(context.Context, uuid.UUID, uuid.UUID, int64) error { return nil }
func (f *oghMemRepo) ListActiveUserIDs(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}
func (f *oghMemRepo) CountActive(context.Context, uuid.UUID) (int, error) { return 0, nil }

var _ port.MembershipRepository = (*oghMemRepo)(nil)

// ── O7-GAP-01: ActiveRoleCodes error after ReassignOwner succeeds → HandleError ─
//
// TestReassignOwner_ActiveRoleCodesError_ReturnsError covers lines 125-128 in
// operator_handler.go:
//
//	roleCodes, err := h.svc.ActiveRoleCodes(ctx, tenantID, tr.UserID)
//	if err != nil { HandleError(c, err); return }
//
// This branch is unreachable via nil-service (absorbPanic), because that
// pattern panics at the first nil dereference (ReassignOwner) before
// ActiveRoleCodes is ever called.
//
// Strategy: wire a real OperatorService whose tenRoles stub
//   - succeeds on Grant (so ReassignOwner returns a *TenantRole), AND
//   - fails on ListByUser (so ActiveRoleCodes returns an error).
func TestReassignOwner_ActiveRoleCodesError_ReturnsError(t *testing.T) {
	tenantID := uuid.New()
	newOwnerID := uuid.New()

	tenants := &oghTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Status: domain.StatusActive}, nil
		},
	}
	roles := &oghRoleRepo{
		// Grant succeeds → ReassignOwner returns a valid *TenantRole.
		grantFn: func(_ context.Context, r *domain.TenantRole) (*domain.TenantRole, error) {
			r.ID = uuid.New()
			return r, nil
		},
		// ListByUser fails → ActiveRoleCodes propagates the error.
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, errors.New("role db read failed")
		},
	}
	mem := &oghMemRepo{
		findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{
				ID: uuid.New(), TenantID: tid, UserID: uid,
				Status: domain.MembershipActive, RecordVersion: 1,
			}, nil
		},
	}

	svc := service.NewOperatorService(tenants, roles, mem, happyCacheStub{}, happyTxRunner{})
	h := &OperatorHandler{svc: svc}

	body := `{"user_id":"` + newOwnerID.String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", tenantID.String())
	h.ReassignOwner(c)

	// The handler must return an error response (not 200), confirming that
	// the HandleError call after ActiveRoleCodes fires.
	assert.NotEqual(t, http.StatusOK, w.Code,
		"ActiveRoleCodes error must prevent a 200 response")
	assert.True(t, w.Code >= 400,
		"expected a 4xx/5xx after ActiveRoleCodes failure, got %d", w.Code)
}

// ── O7-GAP-02: Full success — both ReassignOwner and ActiveRoleCodes succeed ──
//
// TestReassignOwner_FullSuccess_ReturnsRoles covers the full happy path that
// produces a 200 with a `roles` array.  The nil-service (absorbPanic) tests
// in operator_coverage_test.go never reach the JSON-serialisation step because
// they panic on nil dereference.  This test wires real services so the
// response body can be verified.
func TestReassignOwner_FullSuccess_ReturnsRoles(t *testing.T) {
	tenantID := uuid.New()
	newOwnerID := uuid.New()

	tenants := &oghTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Status: domain.StatusActive}, nil
		},
	}
	roles := &oghRoleRepo{
		grantFn: func(_ context.Context, r *domain.TenantRole) (*domain.TenantRole, error) {
			r.ID = uuid.New()
			return r, nil
		},
		listByUserFn: func(_ context.Context, tid, uid uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{
				{TenantID: tid, UserID: uid, RoleCode: domain.RoleTenantOwner},
			}, nil
		},
	}
	mem := &oghMemRepo{
		findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
			return &domain.TenantMembership{
				ID: uuid.New(), TenantID: tid, UserID: uid,
				Status: domain.MembershipActive, RecordVersion: 1,
			}, nil
		},
	}

	svc := service.NewOperatorService(tenants, roles, mem, happyCacheStub{}, happyTxRunner{})
	h := &OperatorHandler{svc: svc}

	body := `{"user_id":"` + newOwnerID.String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", tenantID.String())
	h.ReassignOwner(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"roles"`,
		"response must include the roles array on full success")
	assert.Contains(t, w.Body.String(), "tenant_owner",
		"response roles must contain the newly-granted role code")
}
