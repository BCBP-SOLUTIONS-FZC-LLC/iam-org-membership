// Phase 9 — HTTP handler full-matrix sweep.
//
// Module:   iam-org-membership
// Feature:  All 45 endpoints × Test_prompt.md categories at the handler
//
//	layer — the 22 endpoints Phase 3 didn't cover.
//
// Files:    internal/adapter/inbound/http/*_handler.go
//
// Test IDs: P9-<endpoint-code>-NNN.
//
// Design invariant: every handler gets a `nil` service. If input validation
// leaks to the service on invalid input the test nil-pointer-panics —
// proving the handler's early-return contract.
package http

import (
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ═════════════════════════════════════════════════════════════════════════
// P-3 · DepartmentHandler.List — list active departments for tenant
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-P3-001
// Feature:           P-3 · GET /tenants/{id}/departments — invalid tenant UUID
// Preconditions:     none
// Test Steps:        1. Call List with id path param="not-a-uuid"
// Expected Result:   400 invalid_uuid
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestListDept_InvalidTenantID(t *testing.T) {
	h := &DepartmentHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid")
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-P3-002
// Feature:           P-3 · Missing identity → 401
// Preconditions:     no RequestContext
// Test Steps:        1. Call List without requestctx
// Expected Result:   401 missing_identity_headers
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestListDept_MissingIdentity(t *testing.T) {
	h := &DepartmentHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, nil)
	setParams(c, "id", tenant.String())
	h.List(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// Test Case ID:      P9-P3-003
// Feature:           P-3 · Cross-tenant read blocked → 403
// Preconditions:     caller in tenant B targets tenant A
// Test Steps:        1. Call List with tenant A path, ctx from tenant B
// Expected Result:   403 insufficient_role
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestListDept_CrossTenant(t *testing.T) {
	h := &DepartmentHandler{}
	tenantA := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String())
	h.List(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ═════════════════════════════════════════════════════════════════════════
// P-24 · DepartmentHandler.Activate
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-P24-001
// Feature:           P-24 · POST /tenants/{id}/departments — invalid body
// Preconditions:     valid identity
// Test Steps:        1. Call Activate with malformed JSON
// Expected Result:   400 validation_error
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestActivateDept_MalformedBody(t *testing.T) {
	h := &DepartmentHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodPost, "/", `{`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Activate(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// Test Case ID:      P9-P24-002
// Feature:           P-24 · Cross-tenant activation blocked → 403
// Test Steps:        1. Activate against a different tenant than the caller's
// Expected Result:   403 insufficient_role (before body binding)
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestActivateDept_CrossTenant(t *testing.T) {
	h := &DepartmentHandler{}
	tenantA := uuid.New()
	body := `{"department_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String())
	h.Activate(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// Test Case ID:      P9-P24-003
// Feature:           P-24 · Regular member (non-admin) blocked → 403
// Test Steps:        1. Activate with role=[member] only
// Expected Result:   403 insufficient_role
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestActivateDept_NonAdmin(t *testing.T) {
	h := &DepartmentHandler{}
	tenant := uuid.New()
	rc := &requestctx.RequestContext{
		UserID: uuid.New(), TenantID: tenant, Roles: []string{"member"},
	}
	body := `{"department_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	setParams(c, "id", tenant.String())
	h.Activate(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ═════════════════════════════════════════════════════════════════════════
// P-25 · DepartmentHandler.Patch
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-P25-001
// Feature:           P-25 · Invalid dept UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestPatchDept_InvalidDeptID(t *testing.T) {
	h := &DepartmentHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodPatch, "/", `{"is_active":true,"record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "dept_id", "bogus")
	h.Patch(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-P25-002
// Feature:           P-25 · Cross-tenant patch blocked → 403
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestPatchDept_CrossTenant(t *testing.T) {
	h := &DepartmentHandler{}
	tenantA := uuid.New()
	c, w := buildCtx(http.MethodPatch, "/", `{"is_active":true,"record_version":1}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String(), "dept_id", uuid.New().String())
	h.Patch(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ═════════════════════════════════════════════════════════════════════════
// P-4 · MembershipHandler.List
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-P4-001
// Feature:           P-4 · Invalid tenant UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestListMembers_InvalidTenantID(t *testing.T) {
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bogus")
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-P4-002
// Feature:           P-4 · Cross-tenant list blocked → 403
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestListMembers_CrossTenant(t *testing.T) {
	h := &MembershipHandler{}
	tenantA := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String())
	h.List(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ═════════════════════════════════════════════════════════════════════════
// P-5 · MembershipHandler.Get
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-P5-001
// Feature:           P-5 · Invalid user_id UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestGetMember_InvalidUserID(t *testing.T) {
	h := &MembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", "bogus")
	h.Get(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-P5-002
// Feature:           P-5 · Missing identity → 401
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestGetMember_MissingIdentity(t *testing.T) {
	h := &MembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, nil)
	setParams(c, "id", tenant.String(), "user_id", uuid.New().String())
	h.Get(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// ═════════════════════════════════════════════════════════════════════════
// P-27 · MembershipHandler.SeatUsage
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-P27-001
// Feature:           P-27 · Invalid tenant UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestSeatUsage_InvalidTenantID(t *testing.T) {
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bogus")
	h.SeatUsage(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-P27-002
// Feature:           P-27 · Cross-tenant read blocked → 403
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestSeatUsage_CrossTenant(t *testing.T) {
	h := &MembershipHandler{}
	tenantA := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String())
	h.SeatUsage(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ═════════════════════════════════════════════════════════════════════════
// P-26 · MembershipHandler.RemovalResolution
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-P26-001
// Feature:           P-26 · Invalid user_id → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestRemovalResolution_InvalidUserID(t *testing.T) {
	h := &MembershipHandler{}
	tenant := uuid.New()
	body := `{"action":"stop_workflows"}`
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", "bogus")
	h.RemovalResolution(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-P26-002
// Feature:           P-26 · Malformed body → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestRemovalResolution_MalformedBody(t *testing.T) {
	h := &MembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodPost, "/", `{`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", uuid.New().String())
	h.RemovalResolution(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ═════════════════════════════════════════════════════════════════════════
// P-12 · RoleLabelHandler.List
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-P12-001
// Feature:           P-12 · Invalid tenant UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestListRoleLabels_InvalidTenantID(t *testing.T) {
	h := &RoleLabelHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bogus")
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-P12-002
// Feature:           P-12 · Cross-tenant list → 403
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestListRoleLabels_CrossTenant(t *testing.T) {
	h := &RoleLabelHandler{}
	tenantA := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String())
	h.List(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ═════════════════════════════════════════════════════════════════════════
// P-13 · RoleLabelHandler.Patch
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-P13-001
// Feature:           P-13 · Malformed body → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestPatchRoleLabel_MalformedBody(t *testing.T) {
	h := &RoleLabelHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodPatch, "/", `{`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "role_code", "approver")
	h.Patch(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// Test Case ID:      P9-P13-002
// Feature:           P-13 · Cross-tenant → 403
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestPatchRoleLabel_CrossTenant(t *testing.T) {
	h := &RoleLabelHandler{}
	tenantA := uuid.New()
	body := `{"display_name":"Buyer","record_version":1}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String(), "role_code", "approver")
	h.Patch(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// P-18/P-19/P-20 (ACLHandler) — retired ADR-0007 Wave 3 Phase 6, moved to
// iam-tender-acl's TAC-1/2/3. IDs never reused.

// ═════════════════════════════════════════════════════════════════════════
// O-4 · OperatorHandler.SetFeatureFlags
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-O4-001
// Feature:           O-4 · Non-operator → 403
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP9O4001_SetFeatureFlags_RequiresOperator(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"feature_flags":{"a":true}}`
	c, w := buildCtx(http.MethodPatch, "/", body, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", uuid.New().String())
	h.SetFeatureFlags(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// Test Case ID:      P9-O4-002
// Feature:           O-4 · Invalid tenant UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9O4002_SetFeatureFlags_InvalidID(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"feature_flags":{}}`
	c, w := buildCtx(http.MethodPatch, "/", body, operatorCtx())
	setParams(c, "id", "bogus")
	h.SetFeatureFlags(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}
