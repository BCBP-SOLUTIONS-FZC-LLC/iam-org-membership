// Handler-layer coverage tests for DepartmentHandler (P-24/P-25) — the
// missing-identity/cross-tenant/malformed-input branches not exercised by
// dept_role_happy_test.go's happy paths or p24_p25_coverage_test.go's
// service-error branches.
package http

import (
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
)

// P24-M-03: malformed tenant id path param on Activate → 400 invalid_uuid.
func TestDeptActivate_InvalidTenantID_400(t *testing.T) {
	h := &DepartmentHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{"department_id":"`+uuid.New().String()+`"}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid")
	h.Activate(c)

	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// P24-A-05: no identity in context → 401 missing_identity_headers.
func TestDeptActivate_NoIdentity_401(t *testing.T) {
	h := &DepartmentHandler{}
	tenantID := uuid.New()
	c, w := buildCtx(http.MethodPost, "/", `{"department_id":"`+uuid.New().String()+`"}`, nil)
	setParams(c, "id", tenantID.String())
	h.Activate(c)

	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// P24-A-06: cross-tenant caller → 403 insufficient_role.
func TestDeptActivate_CrossTenant_403(t *testing.T) {
	h := &DepartmentHandler{}
	tenantID := uuid.New()
	rc := &requestctx.RequestContext{UserID: uuid.New(), TenantID: uuid.New(), Roles: []string{"tenant_owner"}}
	c, w := buildCtx(http.MethodPost, "/", `{"department_id":"`+uuid.New().String()+`"}`, rc)
	setParams(c, "id", tenantID.String())
	h.Activate(c)

	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// P25-M-03: malformed tenant id path param on Patch → 400 invalid_uuid.
func TestDeptPatch_InvalidTenantID_400(t *testing.T) {
	h := &DepartmentHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"is_active":false,"record_version":1}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid", "dept_id", uuid.New().String())
	h.Patch(c)

	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// P25-A-05: no identity in context on Patch → 401 missing_identity_headers.
func TestDeptPatch_NoIdentity_401(t *testing.T) {
	h := &DepartmentHandler{}
	tenantID := uuid.New()
	c, w := buildCtx(http.MethodPatch, "/", `{"is_active":false,"record_version":1}`, nil)
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String())
	h.Patch(c)

	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// P25-M-04: malformed JSON body on Patch → 400 validation_error.
func TestDeptPatch_MalformedBody_400(t *testing.T) {
	h := &DepartmentHandler{}
	tenantID := uuid.New()
	c, w := buildCtx(http.MethodPatch, "/", `{"is_active":`, tenantOwnerCtx(tenantID))
	setParams(c, "id", tenantID.String(), "dept_id", uuid.New().String())
	h.Patch(c)

	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}
