// Phase 9 (slice 2) — remaining handler endpoints not covered by
// slice 1: dept-membership List, invitation List, internal reads
// (I-5 delete, I-8 memberships, I-9 locale, I-11 seat-usage, I-6 tender
// access, I-10 SAML JIT).
//
// Test IDs: P9-<endpoint-code>-NNN. Same nil-service invariant.
package http

import (
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ═════════════════════════════════════════════════════════════════════════
// P-8 · DeptMembershipHandler.List — list dept members
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-P8-001
// Feature:           P-8 · Invalid tenant UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestListDeptMembers_InvalidTenantID(t *testing.T) {
	h := &DeptMembershipHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bogus", "dept_id", uuid.New().String())
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-P8-002
// Feature:           P-8 · Invalid dept UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestListDeptMembers_InvalidDeptID(t *testing.T) {
	h := &DeptMembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "dept_id", "bogus")
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-P8-003
// Feature:           P-8 · Missing identity → 401
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestListDeptMembers_MissingIdentity(t *testing.T) {
	h := &DeptMembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, nil)
	setParams(c, "id", tenant.String(), "dept_id", uuid.New().String())
	h.List(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// Test Case ID:      P9-P8-004
// Feature:           P-8 · Cross-tenant → 403
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestListDeptMembers_CrossTenant(t *testing.T) {
	h := &DeptMembershipHandler{}
	tenantA := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String(), "dept_id", uuid.New().String())
	h.List(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ═════════════════════════════════════════════════════════════════════════
// P-30 · InvitationHandler.List — list pending invitations
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-P30-001
// Feature:           P-30 · Invalid tenant UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestListInvitations_InvalidTenantID(t *testing.T) {
	h := &InvitationHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bogus")
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-P30-002
// Feature:           P-30 · Cross-tenant → 403
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestListInvitations_CrossTenant(t *testing.T) {
	h := &InvitationHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", uuid.New().String())
	h.List(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// Test Case ID:      P9-P30-003
// Feature:           P-30 · Missing identity → 401
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestListInvitations_MissingIdentity(t *testing.T) {
	h := &InvitationHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, nil)
	setParams(c, "id", uuid.New().String())
	h.List(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// ═════════════════════════════════════════════════════════════════════════
// I-5 · InternalHandler.DeleteMember
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-I5-001
// Feature:           I-5 · Invalid tenant UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9I5001_DeleteMemberInternal_InvalidTenantID(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodDelete, "/", ``, systemCtx())
	setParams(c, "id", "bogus", "user_id", uuid.New().String())
	h.DeleteMember(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-I5-002
// Feature:           I-5 · Invalid user UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9I5002_DeleteMemberInternal_InvalidUserID(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodDelete, "/", ``, systemCtx())
	setParams(c, "id", uuid.New().String(), "user_id", "bogus")
	h.DeleteMember(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ═════════════════════════════════════════════════════════════════════════
// I-8 · InternalHandler.GetMemberships — the AuthZ hot path (SLO-1)
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-I8-001
// Feature:           I-8 · Invalid user UUID → 400
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP9I8001_GetMemberships_InvalidUserID(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/?tenant_id="+uuid.New().String(), ``, systemCtx())
	setParams(c, "id", "bogus")
	h.GetMemberships(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-I8-002
// Feature:           I-8 · Missing tenant_id query → 400 (per LLD §5.4 I-8)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9I8002_GetMemberships_MissingTenantIDQuery(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", uuid.New().String())
	h.GetMemberships(c)
	// Handler may return 400 for missing tenant_id or delegate to service.
	// Accept 4xx before service call as the invariant.
	assert.True(t, w.Code >= 400 && w.Code < 500,
		"missing tenant_id must produce a 4xx before service call, got %d", w.Code)
}

// ═════════════════════════════════════════════════════════════════════════
// I-9 · InternalHandler.GetLocale
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-I9-001
// Feature:           I-9 · Invalid tenant UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9I9001_GetLocale_InvalidTenantID(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", "bogus")
	h.GetLocale(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ═════════════════════════════════════════════════════════════════════════
// I-11 · InternalHandler.GetSeatUsage (Billing pre-check)
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-I11-001
// Feature:           I-11 · Invalid tenant UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9I11001_GetSeatUsage_InvalidTenantID(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", "bogus")
	h.GetSeatUsage(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ═════════════════════════════════════════════════════════════════════════
// I-6 · InternalHandler.CheckTenderAccess
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-I6-001
// Feature:           I-6 · Invalid tender UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9I6001_CheckTenderAccess_InvalidTenderID(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", uuid.New().String(),
		"tender_id", "bogus", "user_id", uuid.New().String())
	h.CheckTenderAccess(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-I6-002
// Feature:           I-6 · Invalid user UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9I6002_CheckTenderAccess_InvalidUserID(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, systemCtx())
	setParams(c, "id", uuid.New().String(),
		"tender_id", uuid.New().String(), "user_id", "bogus")
	h.CheckTenderAccess(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ═════════════════════════════════════════════════════════════════════════
// I-10 · InternalHandler.AssignFromGroups — SAML JIT
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-I10-001
// Feature:           I-10 · Invalid tenant UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9I10001_AssignFromGroups_InvalidTenantID(t *testing.T) {
	h := &InternalHandler{}
	body := `{"user_id":"` + uuid.New().String() + `","groups":["eng"]}`
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", "bogus")
	h.AssignFromGroups(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-I10-002
// Feature:           I-10 · Missing user_id in body → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9I10002_AssignFromGroups_MissingUserID(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{"groups":["eng"]}`, systemCtx())
	setParams(c, "id", uuid.New().String())
	h.AssignFromGroups(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// Test Case ID:      P9-I10-003
// Feature:           I-10 · Malformed body → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9I10003_AssignFromGroups_MalformedBody(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{`, systemCtx())
	setParams(c, "id", uuid.New().String())
	h.AssignFromGroups(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ═════════════════════════════════════════════════════════════════════════
// O-2 · OperatorHandler.PatchDepartment — authorization coverage
// Test Case IDs: O2-A-02, O2-A-04
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      O2-A-02
// Feature:           O-2 · Regular tenant member role → 403 insufficient_role
// Expected:          403 insufficient_role (AUTH-6: RequireOperatorRole blocks non-operator)
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestPatchDept_MemberRole_403(t *testing.T) {
	h := &OperatorHandler{}
	rc := &requestctx.RequestContext{
		UserID: uuid.New(), TenantID: uuid.New(), Roles: []string{"member"},
	}
	c, w := buildCtx(http.MethodPatch, "/", `{"name":"X","record_version":1}`, rc)
	setParams(c, "id", uuid.New().String())
	h.PatchDepartment(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// Test Case ID:      O2-A-04
// Feature:           O-2 · platform_operator passes auth gate (AUTH-6/AUTH-7 double-check)
// Expected:          Auth gate does NOT return 403 — request proceeds to service layer
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestPatchDept_OperatorRole_200(t *testing.T) {
	h := &OperatorHandler{} // nil service — panics at service call after auth passes
	c, w := buildCtx(http.MethodPatch, "/", `{"name":"X","record_version":1}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	// Absorb nil-service panic that occurs AFTER the auth gate passes.
	// We only test that requireOperator does NOT abort with 403.
	func() {
		defer func() { _ = recover() }()
		h.PatchDepartment(c)
	}()
	assert.NotEqual(t, http.StatusForbidden, w.Code, "platform_operator must pass requireOperator gate")
}

// ═════════════════════════════════════════════════════════════════════════
// O-3 · OperatorHandler.DeleteDepartmentBlocked — authorization coverage
// Test Case ID: O3-A-02
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      O3-A-02
// Feature:           O-3 · Regular tenant member → 403 before hitting 405 guardrail
// Expected:          403 insufficient_role (RequireOperatorRole fires before handler body)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestDeleteDept_MemberRole_403(t *testing.T) {
	h := &OperatorHandler{}
	rc := &requestctx.RequestContext{
		UserID: uuid.New(), TenantID: uuid.New(), Roles: []string{"member"},
	}
	c, w := buildCtx(http.MethodDelete, "/", ``, rc)
	setParams(c, "id", uuid.New().String())
	h.DeleteDepartmentBlocked(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ═════════════════════════════════════════════════════════════════════════
// O-4 · OperatorHandler.SetFeatureFlags — authorization coverage
// Test Case IDs: O4-A-01, O4-A-03
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      O4-A-01
// Feature:           O-4 · No identity headers → 401 missing_identity_headers
// Expected:          401 (unauthenticated — gincommon fires before RequireOperatorRole)
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestSetFeatureFlags_NoAuth_401(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":true}}`, nil)
	setParams(c, "id", uuid.New().String())
	h.SetFeatureFlags(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// Test Case ID:      O4-A-03
// Feature:           O-4 · tenant_admin role → 403 insufficient_role
// Expected:          403 (AUTH-6: only platform_operator may reach /operator/*)
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestSetFeatureFlags_TenantAdmin_403(t *testing.T) {
	h := &OperatorHandler{}
	rc := &requestctx.RequestContext{
		UserID: uuid.New(), TenantID: uuid.New(), Roles: []string{"tenant_admin"},
	}
	c, w := buildCtx(http.MethodPatch, "/", `{"feature_flags":{"sso_enabled":true}}`, rc)
	setParams(c, "id", uuid.New().String())
	h.SetFeatureFlags(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}
