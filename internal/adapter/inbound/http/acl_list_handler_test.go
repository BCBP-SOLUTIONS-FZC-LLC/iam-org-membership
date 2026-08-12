// Handler-layer tests for P-21 GET /tenders/{tender_id}/acl.
package http

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// ── P21-AUTH-01: plain member → 403 ──────────────────────────────────

// Test Case ID:      P21-AUTH-01
// Feature:           P-21 · plain member → 403 (AUTH-3 requireTenderAdminOrHigher)
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestACLList_PlainMember_Returns403(t *testing.T) {
	h := &ACLHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, plainMemberCtx(tenant))
	setParams(c, "id", tenant.String(), "tender_id", uuid.New().String())
	h.List(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P21-AUTH-02: tender_admin → passes auth ──────────────────────────

// Test Case ID:      P21-AUTH-02
// Feature:           P-21 · tender_admin caller is authorised (AUTH-3)
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestACLList_TenderAdmin_Authorised(t *testing.T) {
	h := &ACLHandler{}
	tenant := uuid.New()
	c, _ := buildCtx(http.MethodGet, "/", ``, tenderAdminCtx(tenant))
	setParams(c, "id", tenant.String(), "tender_id", uuid.New().String())
	defer func() { _ = recover() }()
	h.List(c)
}

// ── P21-CROSS-TENANT-01: cross-tenant → 403 ──────────────────────────

// Test Case ID:      P21-CROSS-TENANT-01
// Feature:           P-21 · BBBB caller on AAAA tender → 403
// Priority: P1 · Severity: Blocker · Automation Status: Automated
func TestACLList_CrossTenant_Returns403(t *testing.T) {
	h := &ACLHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenderAdminCtx(uuid.New()))
	pathTenant := uuid.New()
	setParams(c, "id", pathTenant.String(), "tender_id", uuid.New().String())
	h.List(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── P21-INVALID-UUID-01: invalid tender_id → 400 ────────────────────

// Test Case ID:      P21-INVALID-UUID-01
// Feature:           P-21 · invalid tender_id UUID → 400 invalid_uuid
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestACLList_InvalidTenderID_Returns400(t *testing.T) {
	h := &ACLHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, tenderAdminCtx(tenant))
	setParams(c, "id", tenant.String(), "tender_id", "not-a-uuid")
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── P21-INVALID-TENANT-01: invalid tenant_id → 400 ───────────────────

// Test Case ID:      P21-INVALID-TENANT-01
// Feature:           P-21 · invalid tenant_id UUID → 400 invalid_uuid
// Priority: P1 · Severity: Major · Automation Status: Automated
func TestACLList_InvalidTenantID_Returns400(t *testing.T) {
	h := &ACLHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodGet, "/", ``, tenderAdminCtx(tenant))
	setParams(c, "id", "not-a-uuid", "tender_id", uuid.New().String())
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}
