// Handler-layer coverage tests for OperatorHandler (O-4/O-7) that wire a
// real *service.OperatorService with fakes, rather than the nil-service +
// absorbPanic pattern used throughout operator_coverage_test.go — that
// pattern proves validation/auth gates fire early, but never actually
// reaches the handler's post-service-call success/error lines (JSON
// construction, ActiveRoleCodes lookup, etc).
package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// O4-H-FULL-01: full happy path — SetFeatureFlags reaches the tenants repo
// and the handler renders the updated TenantResponse (line 61).
func TestSetFeatureFlags_FullHappyPath_200(t *testing.T) {
	tenantID := uuid.New()
	tenants := &happyTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Slug: "acme", Name: "Acme", RecordVersion: 2}, nil
		},
	}
	svc := service.NewOperatorService(tenants, &mhRoleRepo{}, &mhMemRepo{}, happyCacheStub{}, happyTxRunner{})
	h := &OperatorHandler{svc: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":true},"record_version":1}`, operatorCtx())
	setParams(c, "id", tenantID.String())
	h.SetFeatureFlags(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "acme")
}

// ohSetFeatureFlagsErrRepo overrides SetFeatureFlags on top of
// happyTenantRepo, whose TenantRepositoryNoop default always succeeds.
type ohSetFeatureFlagsErrRepo struct {
	happyTenantRepo
	err error
}

func (r *ohSetFeatureFlagsErrRepo) SetFeatureFlags(context.Context, uuid.UUID, []byte, int64) error {
	return r.err
}

// O4-ERR-01: tenants.SetFeatureFlags (inside RunInTx) surfaces an error
// (optimistic-lock conflict) → propagated via HandleError.
func TestSetFeatureFlags_RepoError_409(t *testing.T) {
	tenantID := uuid.New()
	tenants := &ohSetFeatureFlagsErrRepo{
		err: domain.NewError(domain.ErrOptimisticLockConflict, "record_version mismatch"),
	}
	svc := service.NewOperatorService(tenants, &mhRoleRepo{}, &mhMemRepo{}, happyCacheStub{}, happyTxRunner{})
	h := &OperatorHandler{svc: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":true},"record_version":99}`, operatorCtx())
	setParams(c, "id", tenantID.String())
	h.SetFeatureFlags(c)

	assertErrorCode(t, w, http.StatusConflict, "optimistic_lock_conflict")
}

// O7-H-FULL-01: full happy path — ReassignOwner grants the role, clears
// ownerless_since, and ActiveRoleCodes renders the final response body
// (lines 106-128).
func TestReassignOwner_FullHappyPath_200(t *testing.T) {
	tenantID, newOwnerID := uuid.New(), uuid.New()
	tenants := &happyTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Status: domain.StatusActive}, nil
		},
	}
	mems := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipActive}, nil
	}}
	roles := &p28RoleRepo{
		grantFn: func(_ context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
			return tr, nil
		},
		listByUserFn: func(_ context.Context, tid, uid uuid.UUID) ([]domain.TenantRole, error) {
			return []domain.TenantRole{{TenantID: tid, UserID: uid, RoleCode: domain.RoleTenantOwner}}, nil
		},
	}
	svc := service.NewOperatorService(tenants, roles, mems, happyCacheStub{}, happyTxRunner{})
	h := &OperatorHandler{svc: svc}

	body := `{"user_id":"` + newOwnerID.String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", tenantID.String())
	h.ReassignOwner(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "tenant_owner")
	assert.Contains(t, w.Body.String(), newOwnerID.String())
	assert.Contains(t, w.Body.String(), `"ownerless_since":null`)
}

// O7-ERR-01: svc.ReassignOwner surfaces an error (invalid_owner_candidate)
// → propagated via HandleError (line 108).
func TestReassignOwner_ServiceError_422(t *testing.T) {
	tenantID, newOwnerID := uuid.New(), uuid.New()
	tenants := &happyTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Status: domain.StatusActive}, nil
		},
	}
	mems := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{Status: domain.MembershipSuspended}, nil
	}}
	svc := service.NewOperatorService(tenants, &mhRoleRepo{}, mems, happyCacheStub{}, happyTxRunner{})
	h := &OperatorHandler{svc: svc}

	body := `{"user_id":"` + newOwnerID.String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", tenantID.String())
	h.ReassignOwner(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "invalid_owner_candidate")
}

// O7-ERR-02: ActiveRoleCodes (tenRoles.ListByUser) errors after a
// successful grant → propagated via HandleError (line 116).
func TestReassignOwner_ActiveRoleCodesError_500(t *testing.T) {
	tenantID, newOwnerID := uuid.New(), uuid.New()
	tenants := &happyTenantRepo{
		findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
			return &domain.Tenant{ID: id, Status: domain.StatusActive}, nil
		},
	}
	mems := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipActive}, nil
	}}
	roles := &p28RoleRepo{
		grantFn: func(_ context.Context, tr *domain.TenantRole) (*domain.TenantRole, error) {
			return tr, nil
		},
		listByUserFn: func(context.Context, uuid.UUID, uuid.UUID) ([]domain.TenantRole, error) {
			return nil, domain.NewError(domain.ErrDBUnavailable, "db down")
		},
	}
	svc := service.NewOperatorService(tenants, roles, mems, happyCacheStub{}, happyTxRunner{})
	h := &OperatorHandler{svc: svc}

	body := `{"user_id":"` + newOwnerID.String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", tenantID.String())
	h.ReassignOwner(c)

	assertErrorCode(t, w, http.StatusServiceUnavailable, "db_unavailable")
}
