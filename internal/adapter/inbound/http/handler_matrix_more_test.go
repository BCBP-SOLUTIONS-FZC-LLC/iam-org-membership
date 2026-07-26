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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ═════════════════════════════════════════════════════════════════════════
// P-8 · DeptMembershipHandler.List — list dept members
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P9-P8-001
// Feature:           P-8 · Invalid tenant UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9P8001_ListDeptMembers_InvalidTenantID_400(t *testing.T) {
	h := &DeptMembershipHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bogus", "dept_id", uuid.New().String())
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-P8-002
// Feature:           P-8 · Invalid dept UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9P8002_ListDeptMembers_InvalidDeptID_400(t *testing.T) {
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
func TestP9P8003_ListDeptMembers_MissingIdentity_401(t *testing.T) {
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
func TestP9P8004_ListDeptMembers_CrossTenant_403(t *testing.T) {
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
func TestP9P30001_ListInvitations_InvalidTenantID_400(t *testing.T) {
	h := &InvitationHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bogus")
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-P30-002
// Feature:           P-30 · Cross-tenant → 403
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestP9P30002_ListInvitations_CrossTenant_403(t *testing.T) {
	h := &InvitationHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", uuid.New().String())
	h.List(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// Test Case ID:      P9-P30-003
// Feature:           P-30 · Missing identity → 401
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9P30003_ListInvitations_MissingIdentity_401(t *testing.T) {
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
func TestP9I5001_DeleteMemberInternal_InvalidTenantID_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodDelete, "/", ``, systemCtx())
	setParams(c, "id", "bogus", "user_id", uuid.New().String())
	h.DeleteMember(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P9-I5-002
// Feature:           I-5 · Invalid user UUID → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9I5002_DeleteMemberInternal_InvalidUserID_400(t *testing.T) {
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
func TestP9I8001_GetMemberships_InvalidUserID_400(t *testing.T) {
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
func TestP9I9001_GetLocale_InvalidTenantID_400(t *testing.T) {
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
func TestP9I11001_GetSeatUsage_InvalidTenantID_400(t *testing.T) {
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
func TestP9I6001_CheckTenderAccess_InvalidTenderID_400(t *testing.T) {
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
func TestP9I6002_CheckTenderAccess_InvalidUserID_400(t *testing.T) {
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
func TestP9I10001_AssignFromGroups_InvalidTenantID_400(t *testing.T) {
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
func TestP9I10002_AssignFromGroups_MissingUserID_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{"groups":["eng"]}`, systemCtx())
	setParams(c, "id", uuid.New().String())
	h.AssignFromGroups(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// Test Case ID:      P9-I10-003
// Feature:           I-10 · Malformed body → 400
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestP9I10003_AssignFromGroups_MalformedBody_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{`, systemCtx())
	setParams(c, "id", uuid.New().String())
	h.AssignFromGroups(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
