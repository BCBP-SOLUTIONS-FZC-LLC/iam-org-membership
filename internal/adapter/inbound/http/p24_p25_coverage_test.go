// Handler-layer unit tests for:
//
//	P-24 POST /api/v1/tenants/{id}/departments         (DepartmentHandler.Activate)
//	P-25 PATCH /api/v1/tenants/{id}/departments/{id}   (DepartmentHandler.Patch)
//
// Complements handler_matrix_test.go (malformed/cross-tenant/non-admin) and
// dept_role_happy_test.go (happy paths) with service-error and edge branches.
package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
)

// ── P-24 DepartmentHandler.Activate ──────────────────────────────────────────

// P24-M-02: empty body (no department_id) → 400 invalid_uuid.
func TestDeptActivate_MissingDeptID_400(t *testing.T) {
	tenantID := uuid.New()
	svc := service.NewDepartmentService(&drhDeptRepo{}, &drhTenantDeptRepo{}, drhCache{})
	h := &DepartmentHandler{svc: svc}

	c, w := buildCtx(http.MethodPost, "/", `{}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.Activate(c)

	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// P24-AUTH-04: tender_admin → 403 (AUTH-2 excludes tender_admin).
func TestDeptActivate_TenderAdminForbidden_403(t *testing.T) {
	tenantID := uuid.New()
	h := &DepartmentHandler{}

	rc := &requestctx.RequestContext{
		UserID: uuid.New(), TenantID: tenantID, Roles: []string{"tender_admin"},
	}
	c, w := buildCtx(http.MethodPost, "/", `{"department_id":"`+uuid.New().String()+`"}`, rc)
	setParams(c, "id", tenantID.String())
	h.Activate(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// P24-IDEMPOTENT-01: repo returns ErrDepartmentAlreadyActivated → 200 idempotent (LLD §5.4 P-24).
func TestDeptActivate_AlreadyActivated_409(t *testing.T) {
	tenantID := uuid.New()
	deptID := uuid.New()
	existing := &domain.TenantDepartment{DepartmentID: deptID, IsActive: true, RecordVersion: 1}
	td := &drhTenantDeptRepo{
		activateFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
			return nil, domain.NewError(domain.ErrDepartmentAlreadyActivated,
				"department is already activated for this tenant")
		},
		findFn: func(_ context.Context, _, _ uuid.UUID) (*domain.TenantDepartment, error) {
			return existing, nil
		},
	}
	svc := service.NewDepartmentService(&drhDeptRepo{}, td, drhCache{})
	h := &DepartmentHandler{svc: svc}

	body := `{"department_id":"` + deptID.String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.Activate(c)

	// P-24 is idempotent: already-active → 200 with existing row, not 409.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 idempotent, got %d: %s", w.Code, w.Body.String())
	}
}

// P24-422-01: catalog dept is globally retired → 422 department_retired (Bug B-12 fix).
// The service's D-5/TD-1 check reads dept.IsActive and returns ErrDepartmentRetired.
func TestDeptActivate_RetiredDept_422(t *testing.T) {
	tenantID := uuid.New()
	deptID := uuid.New()
	// drhDeptRepo.FindByID default returns IsActive=true; override to false.
	catalog := &drhDeptRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
		return &domain.Department{ID: id, Code: "RETIRED", Name: "Retired", IsActive: false}, nil
	}}
	svc := service.NewDepartmentService(catalog, &drhTenantDeptRepo{}, drhCache{})
	h := &DepartmentHandler{svc: svc}

	body := `{"department_id":"` + deptID.String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.Activate(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "department_retired")
}

// P24-NF-01: unknown department_id not in global catalog → 404.
func TestDeptActivate_UnknownDeptID_404(t *testing.T) {
	tenantID := uuid.New()
	catalog := &drhDeptRepo{findByIDFn: func(context.Context, uuid.UUID) (*domain.Department, error) {
		return nil, domain.NewError(domain.ErrDepartmentNotFound, "department not found")
	}}
	svc := service.NewDepartmentService(catalog, &drhTenantDeptRepo{}, drhCache{})
	h := &DepartmentHandler{svc: svc}

	body := `{"department_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.Activate(c)

	assertErrorCode(t, w, http.StatusNotFound, "department_not_found")
}

// ── P-25 DepartmentHandler.Patch — additional branches ───────────────────────

// P25-CONC-01: repo returns ErrOptimisticLockConflict → 409 (CONC-4).
func TestDeptPatch_OptimisticLockConflict_409(t *testing.T) {
	tenantID := uuid.New()
	deptID := uuid.New()
	td := &drhTenantDeptRepo{setActiveFn: func(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
		return nil, domain.NewError(domain.ErrOptimisticLockConflict, "record version conflict")
	}}
	svc := service.NewDepartmentService(&drhDeptRepo{}, td, drhCache{})
	h := &DepartmentHandler{svc: svc}

	body := `{"is_active":false,"record_version":99}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", deptID.String())
	h.Patch(c)

	assertErrorCode(t, w, http.StatusConflict, "optimistic_lock_conflict")
}

// P25-422-01: deactivate a system department → 422 system_department_cannot_be_retired.
// This used to be enforced by the departments table's chk_system_department_active
// constraint; that table now lives in the Catalog Service (migration-runbook
// Phase 4), so DepartmentService.SetActive checks dept.IsSystem itself against
// the fetched catalog record and returns ErrSystemDepartmentCannotBeRetired directly.
func TestDeptPatch_SystemDeptRetire_422(t *testing.T) {
	tenantID := uuid.New()
	deptID := uuid.New()
	td := &drhTenantDeptRepo{setActiveFn: func(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
		return nil, domain.NewError(domain.ErrSystemDepartmentCannotBeRetired,
			"system department cannot be deactivated")
	}}
	svc := service.NewDepartmentService(&drhDeptRepo{}, td, drhCache{})
	h := &DepartmentHandler{svc: svc}

	body := `{"is_active":false,"record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", deptID.String())
	h.Patch(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "system_department_cannot_be_retired")
}

// P25-NF-01: dept not activated for tenant → 404.
func TestDeptPatch_NotActivated_404(t *testing.T) {
	tenantID := uuid.New()
	deptID := uuid.New()
	td := &drhTenantDeptRepo{setActiveFn: func(context.Context, uuid.UUID, uuid.UUID, bool, int64) (*domain.TenantDepartment, error) {
		return nil, domain.NewError(domain.ErrDepartmentNotFound, "department not found")
	}}
	svc := service.NewDepartmentService(&drhDeptRepo{}, td, drhCache{})
	h := &DepartmentHandler{svc: svc}

	body := `{"is_active":false,"record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", deptID.String())
	h.Patch(c)

	assertErrorCode(t, w, http.StatusNotFound, "department_not_found")
}

// P25-422-02: reactivate (is_active=true) a globally retired dept → 422 department_retired (TD-1/D-5, Bug B-14 fix).
func TestDeptPatch_ReactivateRetiredDept_422(t *testing.T) {
	tenantID := uuid.New()
	deptID := uuid.New()
	catalog := &drhDeptRepo{findByIDFn: func(_ context.Context, id uuid.UUID) (*domain.Department, error) {
		return &domain.Department{ID: id, Code: "RETIRED", Name: "Retired", IsActive: false, IsSystem: false}, nil
	}}
	svc := service.NewDepartmentService(catalog, &drhTenantDeptRepo{}, drhCache{})
	h := &DepartmentHandler{svc: svc}

	body := `{"is_active":true,"record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", deptID.String())
	h.Patch(c)

	assertErrorCode(t, w, http.StatusUnprocessableEntity, "department_retired")
}

// P25-M-02: missing is_active field → 400.
func TestDeptPatch_MissingIsActive_400(t *testing.T) {
	tenantID := uuid.New()
	svc := service.NewDepartmentService(&drhDeptRepo{}, &drhTenantDeptRepo{}, drhCache{})
	h := &DepartmentHandler{svc: svc}

	c, w := buildCtx(http.MethodPatch, "/", `{"record_version":1}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String())
	h.Patch(c)

	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}
