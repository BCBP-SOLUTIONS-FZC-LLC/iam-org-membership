// Handler-layer coverage tests for RoleLabelHandler (P-12/P-13) — the two
// branches not exercised by dept_role_happy_test.go's happy paths: List's
// service-error passthrough, and Patch's invalid-tenant-id-path branch.
package http

import (
	"context"
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
)

// P12-ERR-01: svc.List returns an error → propagated via HandleError.
func TestRoleLabelList_ServiceError(t *testing.T) {
	tenantID := uuid.New()
	repo := &drhLabelRepo{listFn: func(context.Context, uuid.UUID) ([]domain.DeptRoleLabel, error) {
		return nil, domain.NewError(domain.ErrTenantNotFound, "tenant not found")
	}}
	svc := service.NewRoleLabelService(repo, drhCache{})
	h := &RoleLabelHandler{svc: svc}

	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String())
	h.List(c)

	assertErrorCode(t, w, http.StatusNotFound, "tenant_not_found")
}

// P13-M-03: malformed tenant id path param on Patch → 400 invalid_uuid.
func TestRoleLabelPatch_InvalidTenantID_400(t *testing.T) {
	h := &RoleLabelHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"display_name":"X","record_version":1}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid", "role_code", "approver")
	h.Patch(c)

	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}
