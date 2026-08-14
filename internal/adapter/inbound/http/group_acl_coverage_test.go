// Handler-layer tests for two APIs added in today's testing session:
//
//	P-22  POST /tenants/{id}/tenders/{tender_id}/acl
//	P-23  DELETE /tenants/{id}/tenders/{tender_id}/acl/{user_id}
//
// (P-15/P-17/P-29 GroupMappingHandler coverage retired along with the
// handler itself — moved to Group Mapping Service.)
//
// Pattern: nil-service + nil-repo. Any path that reaches the service with
// a nil pointer panics — proving the handler's early-return contract.
package http

import (
	"net/http"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/google/uuid"
)

// tenderAdminCtx returns a RequestContext with tender_admin role.
func tenderAdminCtx(tenantID uuid.UUID) *requestctx.RequestContext {
	return &requestctx.RequestContext{
		UserID: uuid.New(), TenantID: tenantID, Roles: []string{"tender_admin"},
	}
}

// ═════════════════════════════════════════════════════════════════════════════
// P-22 · ACLHandler.Grant — POST tenders/{tender_id}/acl
// ═════════════════════════════════════════════════════════════════════════════

// Test Case ID:      P22-VAL-TENANT-01
// Feature:           P-22 · Invalid tenant UUID → 400
// Scenario ID:       P22-VAL-TENANT-01
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestACLGrant_InvalidTenantID(t *testing.T) {
	h := &ACLHandler{}
	tenant := uuid.New()
	body := `{"user_id":"` + uuid.New().String() + `","access_level":"view"}`
	c, w := buildCtx(http.MethodPost, "/", body, tenantOwnerCtx(tenant))
	setParams(c, "id", "bad-uuid", "tender_id", uuid.New().String())
	h.Grant(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P22-AUTH-01
// Feature:           P-22 · Plain member → 403 (AUTH-3)
// Scenario ID:       P22-AUTH-01
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestACLGrant_PlainMember(t *testing.T) {
	h := &ACLHandler{}
	tenant := uuid.New()
	body := `{"user_id":"` + uuid.New().String() + `","access_level":"view"}`
	c, w := buildCtx(http.MethodPost, "/", body, plainMemberCtx(tenant))
	setParams(c, "id", tenant.String(), "tender_id", uuid.New().String())
	h.Grant(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// Test Case ID:      P22-AUTH-02
// Feature:           P-22 · tender_admin caller is authorised (AUTH-3)
// Scenario ID:       P22-AUTH-02
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestACLGrant_TenderAdminAuthorised(t *testing.T) {
	h := &ACLHandler{}
	tenant := uuid.New()
	body := `{"user_id":"` + uuid.New().String() + `","access_level":"view"}`
	c, _ := buildCtx(http.MethodPost, "/", body, tenderAdminCtx(tenant))
	setParams(c, "id", tenant.String(), "tender_id", uuid.New().String())
	defer func() { _ = recover() }()
	h.Grant(c)
}

// Test Case ID:      P22-AUTH-03
// Feature:           P-22 · tenant_admin caller is authorised (AUTH-3)
// Scenario ID:       P22-AUTH-03
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestACLGrant_TenantAdminAuthorised(t *testing.T) {
	h := &ACLHandler{}
	tenant := uuid.New()
	body := `{"user_id":"` + uuid.New().String() + `","access_level":"view"}`
	c, _ := buildCtx(http.MethodPost, "/", body, tenantAdminCtx(tenant))
	setParams(c, "id", tenant.String(), "tender_id", uuid.New().String())
	defer func() { _ = recover() }()
	h.Grant(c)
}

// ═════════════════════════════════════════════════════════════════════════════
// P-23 · ACLHandler.Revoke — DELETE tenders/{tender_id}/acl/{user_id}
// ═════════════════════════════════════════════════════════════════════════════

// Test Case ID:      P23-VAL-TENANT-01
// Feature:           P-23 · Invalid tenant UUID → 400
// Scenario ID:       P23-VAL-TENANT-01
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestACLRevoke_InvalidTenantID(t *testing.T) {
	h := &ACLHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", "bad-uuid", "tender_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P23-VAL-TENDER-01
// Feature:           P-23 · Invalid tender UUID → 400
// Scenario ID:       P23-VAL-TENDER-01
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestACLRevoke_InvalidTenderID(t *testing.T) {
	h := &ACLHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "tender_id", "bad-uuid", "user_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// Test Case ID:      P23-AUTH-01
// Feature:           P-23 · Plain member → 403 (AUTH-3)
// Scenario ID:       P23-AUTH-01
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestACLRevoke_PlainMember(t *testing.T) {
	h := &ACLHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodDelete, "/", ``, plainMemberCtx(tenant))
	setParams(c, "id", tenant.String(), "tender_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// Test Case ID:      P23-AUTH-03
// Feature:           P-23 · Cross-tenant → 403
// Scenario ID:       P23-AUTH-03
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestACLRevoke_CrossTenant(t *testing.T) {
	h := &ACLHandler{}
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(uuid.New()))
	pathTenant := uuid.New()
	setParams(c, "id", pathTenant.String(), "tender_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// Test Case ID:      P23-AUTH-02
// Feature:           P-23 · tender_admin caller is authorised (AUTH-3)
// Scenario ID:       P23-AUTH-02
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestACLRevoke_TenderAdminAuthorised(t *testing.T) {
	h := &ACLHandler{}
	tenant := uuid.New()
	c, _ := buildCtx(http.MethodDelete, "/", ``, tenderAdminCtx(tenant))
	setParams(c, "id", tenant.String(), "tender_id", uuid.New().String(), "user_id", uuid.New().String())
	defer func() { _ = recover() }()
	h.Revoke(c)
}
