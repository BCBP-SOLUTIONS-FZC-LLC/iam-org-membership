// Handler-layer coverage tests for DeptMembershipHandler (P-9/P-10/P-11):
// branches not already exercised by dept_role_happy_test.go's happy paths —
// malformed input on Assign, and the service-error passthrough branches on
// List/Remove.
package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
)

// P9-ERR-01: ListByDepartment returns a service error → propagated (404 department_not_found).
func TestDeptMembersList_ServiceError_404(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	td := &drhTenantDeptRepo{findFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantDepartment, error) {
		return nil, domain.NewError(domain.ErrDepartmentNotFound, "department not found or not activated for this tenant")
	}}
	svc := service.NewDeptMembershipService(&drhDeptMemRepo{}, &drhMemRepo{}, td, nil, nil, &drhWorkflowClient{}, drhCache{}, drhTxRunner{})
	h := &DeptMembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", deptID.String())
	h.List(c)

	assertErrorCode(t, w, http.StatusNotFound, "department_not_found")
}

// P10-M-02: malformed tenant id path param on Assign → 400 invalid_uuid.
func TestDeptMembersAssign_InvalidTenantID_400(t *testing.T) {
	h := &DeptMembershipHandler{}
	c, w := buildCtx(http.MethodPut, "/", `{"level":"preparator"}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid", "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Assign(c)

	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// P10-M-03: malformed dept_id path param → 400 invalid_uuid.
func TestDeptMembersAssign_InvalidDeptID_400(t *testing.T) {
	tenantID := uuid.New()
	h := &DeptMembershipHandler{}

	c, w := buildCtx(http.MethodPut, "/", `{"level":"preparator"}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", "not-a-uuid", "user_id", uuid.New().String())
	h.Assign(c)

	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// P10-M-04: malformed user_id path param → 400 invalid_uuid.
func TestDeptMembersAssign_InvalidUserID_400(t *testing.T) {
	tenantID := uuid.New()
	h := &DeptMembershipHandler{}

	c, w := buildCtx(http.MethodPut, "/", `{"level":"preparator"}`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", "not-a-uuid")
	h.Assign(c)

	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// P10-M-05: malformed JSON body → 400 validation_error.
func TestDeptMembersAssign_MalformedBody_400(t *testing.T) {
	tenantID := uuid.New()
	svc := service.NewDeptMembershipService(&drhDeptMemRepo{}, &drhMemRepo{}, &drhTenantDeptRepo{}, nil, nil, &drhWorkflowClient{}, drhCache{}, drhTxRunner{})
	h := &DeptMembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodPut, "/", `{"level":`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Assign(c)

	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// P11-AUTH-01: caller lacking tenant_admin/owner → 403 (requireTenantAdmin gate).
func TestDeptMembersRemove_NotTenantAdmin_403(t *testing.T) {
	tenantID := uuid.New()
	h := &DeptMembershipHandler{}

	c, w := buildCtx(http.MethodDelete, "/", ``, plainMemberCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Remove(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// P11-ERR-01: svc.Remove surfaces a workflow_resolution_required 409 (§8.8.4).
func TestDeptMembersRemove_WorkflowResolutionRequired_409(t *testing.T) {
	tenantID, deptID, userID := uuid.New(), uuid.New(), uuid.New()
	dm := &drhDeptMemRepo{removeFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.DeptMembership, error) {
		return nil, domain.NewError(domain.ErrWorkflowResolutionRequired, "active workflows must be resolved").
			WithDetails(map[string]any{"active_workflows": 1})
	}}
	svc := service.NewDeptMembershipService(dm, &drhMemRepo{}, &drhTenantDeptRepo{}, nil, nil, &drhWorkflowClient{}, drhCache{}, drhTxRunner{})
	h := &DeptMembershipHandler{svc: svc}

	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", deptID.String(), "user_id", userID.String())
	h.Remove(c)

	assertErrorCode(t, w, http.StatusConflict, "workflow_resolution_required")
}
