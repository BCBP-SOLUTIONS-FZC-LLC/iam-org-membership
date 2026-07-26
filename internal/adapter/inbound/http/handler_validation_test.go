// Phase 3 — Handler-layer validation tests for the 11 event-firing
// endpoints. Focus: input rejected BEFORE the service is invoked
// (malformed body, invalid path UUID, missing identity, wrong role,
// cross-tenant). Because the handler exits early on validation failure,
// we can pass a nil service — any accidental reach to the service would
// nil-pointer-panic, giving us a strong "handler doesn't leak to service
// on invalid input" contract.
//
// Happy-path tests requiring service execution live in the postgres/
// integration suite (Phase 4).
//
// Pattern for every test:
//
//  1. Build a gin.Context with newTestContext (already in middleware_test.go)
//  2. Populate c.Params for URL path variables
//  3. Populate the body via c.Request = httptest.NewRequest with a reader
//  4. Call handler method directly
//  5. Assert on w.Code + w.Body
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── shared test helpers ────────────────────────────────────────────────

// tenantOwnerCtx returns a RequestContext for a user with tenant_owner in
// the given tenant. Convenient for tests that need to pass the
// requireTenantAdmin gate.
func tenantOwnerCtx(tenantID uuid.UUID) *requestctx.RequestContext {
	return &requestctx.RequestContext{
		UserID:   uuid.New(),
		TenantID: tenantID,
		Roles:    []string{"tenant_owner"},
	}
}

// operatorCtx returns a RequestContext with platform_operator role. Cross-
// tenant, so TenantID is a zero UUID.
func operatorCtx() *requestctx.RequestContext {
	return &requestctx.RequestContext{
		UserID: uuid.New(),
		Roles:  []string{"platform_operator"},
	}
}

// systemCtx returns a RequestContext for the iam-system principal used by
// internal-lane callers (RP webhook, Workflow, etc.).
func systemCtx() *requestctx.RequestContext {
	return &requestctx.RequestContext{
		UserID: uuid.New(),
		Roles:  []string{"iam-system"},
	}
}

// buildCtx wraps newTestContext with an HTTP method, path, and body reader
// so each test can express the request shape declaratively.
func buildCtx(method, path, body string, rc *requestctx.RequestContext) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if rc != nil {
		req = req.WithContext(requestctx.WithContext(req.Context(), rc))
	}
	c.Request = req
	return c, w
}

// setParams populates c.Params (URL path variables) since httptest doesn't
// know the router's path pattern.
func setParams(c *gin.Context, kvs ...string) {
	if len(kvs)%2 != 0 {
		panic("setParams needs an even number of args (key1, val1, key2, val2, ...)")
	}
	for i := 0; i < len(kvs); i += 2 {
		c.Params = append(c.Params, gin.Param{Key: kvs[i], Value: kvs[i+1]})
	}
}

// assertErrorCode extracts the `code` from a JSON error body and asserts
// it matches. Also verifies the HTTP status.
func assertErrorCode(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	assert.Equal(t, wantStatus, w.Code, "http status")
	if w.Body == nil || w.Body.Len() == 0 {
		return
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err == nil {
		assert.Equal(t, wantCode, body["code"], "error code in body")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// I-1: POST /internal/tenants — ProvisionTenant
// ─────────────────────────────────────────────────────────────────────────

func TestProvisionTenant_MalformedJSON_400(t *testing.T) {
	h := &InternalHandler{} // nil svc — validation must fail before service
	c, w := buildCtx(http.MethodPost, "/api/v1/internal/tenants", `{"slug":`, systemCtx())
	h.ProvisionTenant(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

func TestProvisionTenant_EmptyBody_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPost, "/api/v1/internal/tenants", ``, systemCtx())
	h.ProvisionTenant(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestProvisionTenant_MissingRequiredFields_400(t *testing.T) {
	// Body parses as JSON but violates the DTO shape (missing required fields).
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPost, "/api/v1/internal/tenants", `{}`, systemCtx())
	h.ProvisionTenant(c)
	// Depending on which validator kicks first, this is either 400 or 422.
	// We assert only that it's a 4xx and never reaches service.
	assert.True(t, w.Code >= 400 && w.Code < 500, "expected 4xx, got %d", w.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// I-2: PATCH /internal/tenants/{id} — PatchTenantRealm
// ─────────────────────────────────────────────────────────────────────────

func TestPatchTenantRealm_InvalidTenantID_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPatch, "/api/v1/internal/tenants/not-a-uuid", `{"realm_id":"acme"}`, systemCtx())
	setParams(c, "id", "not-a-uuid")
	h.PatchTenantRealm(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestPatchTenantRealm_MalformedBody_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPatch, "/api/v1/internal/tenants/"+uuid.New().String(), `{"broken`, systemCtx())
	setParams(c, "id", uuid.New().String())
	h.PatchTenantRealm(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// I-3: POST /internal/tenants/{id}/members — AddMember (invitation accept)
// ─────────────────────────────────────────────────────────────────────────

func TestAddMember_InvalidTenantID_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{"user_id":"aaaa1111-1111-1111-1111-111111111111"}`, systemCtx())
	setParams(c, "id", "bogus")
	h.AddMember(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestAddMember_MissingUserID_400(t *testing.T) {
	// Body parses fine but user_id is required. Handler must 400 before
	// touching the InvitationService.
	h := &InternalHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodPost, "/", `{}`, systemCtx())
	setParams(c, "id", tenant.String())
	h.AddMember(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

func TestAddMember_MalformedJSON_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{"user_id`, systemCtx())
	setParams(c, "id", uuid.New().String())
	h.AddMember(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// I-4: PATCH /internal/tenants/{id}/members/{user_id} — PatchMemberLifecycle
// ─────────────────────────────────────────────────────────────────────────

func TestPatchMemberLifecycle_InvalidUserID_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"status":"suspended","record_version":1}`, systemCtx())
	setParams(c, "id", uuid.New().String(), "user_id", "not-a-uuid")
	h.PatchMemberLifecycle(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ─────────────────────────────────────────────────────────────────────────
// I-13: POST /internal/tenants/{id}/tenders/{tender_id}/assignee-override
// ─────────────────────────────────────────────────────────────────────────

func TestAssigneeOverride_InvalidTenderID_400(t *testing.T) {
	h := &InternalHandler{}
	body := `{"new_user_id":"` + uuid.New().String() + `","department_id":"` + uuid.New().String() +
		`","required_level":"reviewer","actor_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, systemCtx())
	setParams(c, "id", uuid.New().String(), "tender_id", "not-a-uuid")
	h.AssigneeOverride(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestAssigneeOverride_MalformedBody_400(t *testing.T) {
	h := &InternalHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{`, systemCtx())
	setParams(c, "id", uuid.New().String(), "tender_id", uuid.New().String())
	h.AssigneeOverride(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// P-6: POST /tenants/{id}/members — Invite (invitation staging)
// ─────────────────────────────────────────────────────────────────────────

func TestInvite_MissingIdentity_401(t *testing.T) {
	h := &InvitationHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodPost, "/", `{"email":"u@e.com","full_name":"U"}`, nil)
	setParams(c, "id", tenant.String())
	h.Invite(c)
	// requireTenantAdmin returns ErrMissingIdentity → 401
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

func TestInvite_CrossTenant_403(t *testing.T) {
	h := &InvitationHandler{}
	tenantA := uuid.New()
	tenantB := uuid.New()
	rc := tenantOwnerCtx(tenantB) // caller belongs to B, targets A
	c, w := buildCtx(http.MethodPost, "/", `{"email":"u@e.com","full_name":"U"}`, rc)
	setParams(c, "id", tenantA.String())
	h.Invite(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

func TestInvite_InsufficientRole_403(t *testing.T) {
	h := &InvitationHandler{}
	tenant := uuid.New()
	// A regular member (no tenant_admin/tenant_owner) hitting an admin route.
	rc := &requestctx.RequestContext{
		UserID: uuid.New(), TenantID: tenant, Roles: []string{"member"},
	}
	c, w := buildCtx(http.MethodPost, "/", `{"email":"u@e.com","full_name":"U"}`, rc)
	setParams(c, "id", tenant.String())
	h.Invite(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

func TestInvite_InvalidTenantID_400(t *testing.T) {
	h := &InvitationHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{"email":"u@e.com","full_name":"U"}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid")
	h.Invite(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ─────────────────────────────────────────────────────────────────────────
// P-31: DELETE /tenants/{id}/invitations/{invitation_id} — Revoke
// (B9/B10/B19 — status code, body binding, query fallback)
// ─────────────────────────────────────────────────────────────────────────

func TestRevoke_InvalidInvitationID_400(t *testing.T) {
	h := &InvitationHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "invitation_id", "bogus")
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestRevoke_CrossTenant_403(t *testing.T) {
	h := &InvitationHandler{}
	tenantA := uuid.New()
	c, w := buildCtx(http.MethodDelete, "/", `{"record_version":1}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String(), "invitation_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ─────────────────────────────────────────────────────────────────────────
// P-28: PUT /tenants/{id}/members/{user_id}/roles — ReconcileRoles
// ─────────────────────────────────────────────────────────────────────────

func TestReconcileRoles_InvalidUserID_400(t *testing.T) {
	h := &MembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodPut, "/", `{"roles":["tender_admin"]}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", "bogus")
	h.ReconcileRoles(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestReconcileRoles_MissingIdentity_401(t *testing.T) {
	h := &MembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodPut, "/", `{"roles":["tender_admin"]}`, nil)
	setParams(c, "id", tenant.String(), "user_id", uuid.New().String())
	h.ReconcileRoles(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// ─────────────────────────────────────────────────────────────────────────
// P-10: PUT /tenants/{id}/departments/{dept_id}/members/{user_id} — Assign
// ─────────────────────────────────────────────────────────────────────────

func TestDeptAssign_InvalidDeptID_400(t *testing.T) {
	h := &DeptMembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodPut, "/", `{"level":"reviewer"}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "dept_id", "bogus", "user_id", uuid.New().String())
	h.Assign(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestDeptAssign_CrossTenant_403(t *testing.T) {
	h := &DeptMembershipHandler{}
	tenantA := uuid.New()
	c, w := buildCtx(http.MethodPut, "/", `{"level":"reviewer"}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String(), "dept_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Assign(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ─────────────────────────────────────────────────────────────────────────
// P-11: DELETE /tenants/{id}/departments/{dept_id}/members/{user_id} — Remove
// ─────────────────────────────────────────────────────────────────────────

func TestDeptRemove_InvalidUserID_400(t *testing.T) {
	h := &DeptMembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "dept_id", uuid.New().String(), "user_id", "bogus")
	h.Remove(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ─────────────────────────────────────────────────────────────────────────
// P-14/P-15: /delegations — Create, End
// ─────────────────────────────────────────────────────────────────────────

func TestDelegationCreate_MissingIdentity_401(t *testing.T) {
	h := &DelegationHandler{}
	body := `{"delegate_id":"` + uuid.New().String() + `","scope":"all"}`
	c, w := buildCtx(http.MethodPost, "/", body, nil)
	h.Create(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

func TestDelegationCreate_MalformedBody_400(t *testing.T) {
	h := &DelegationHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{`, tenantOwnerCtx(uuid.New()))
	h.Create(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDelegationEnd_InvalidDelegationID_400(t *testing.T) {
	h := &DelegationHandler{}
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid")
	h.Cancel(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ─────────────────────────────────────────────────────────────────────────
// O-7: POST /operator/tenants/{id}/reassign-owner
// ─────────────────────────────────────────────────────────────────────────

func TestReassignOwner_RequiresOperator_403(t *testing.T) {
	h := &OperatorHandler{}
	// tenant_owner is NOT enough for operator routes (AUTH-6).
	rc := tenantOwnerCtx(uuid.New())
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, rc)
	setParams(c, "id", uuid.New().String())
	h.ReassignOwner(c)
	// The handler's own requireOperator check runs after param parse; must 403.
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

func TestReassignOwner_InvalidTenantID_400(t *testing.T) {
	h := &OperatorHandler{}
	body := `{"user_id":"` + uuid.New().String() + `"}`
	c, w := buildCtx(http.MethodPost, "/", body, operatorCtx())
	setParams(c, "id", "not-a-uuid")
	h.ReassignOwner(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestReassignOwner_MissingUserID_400(t *testing.T) {
	// N3-related: neither `user_id` nor `new_owner_user_id` supplied.
	// EffectiveUserID returns uuid.Nil → handler must 400 before service.
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPost, "/", `{}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	h.ReassignOwner(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

// ─────────────────────────────────────────────────────────────────────────
// P-7 + P-26: Member remove + removal-resolution
// ─────────────────────────────────────────────────────────────────────────

func TestMemberRemove_InvalidUserID_400(t *testing.T) {
	h := &MembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", "bogus")
	h.Remove(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestMemberRemove_CrossTenant_403(t *testing.T) {
	h := &MembershipHandler{}
	tenantA := uuid.New()
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String(), "user_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ─────────────────────────────────────────────────────────────────────────
// P-1 / P-2: Tenant read / patch (non-event but common)
// ─────────────────────────────────────────────────────────────────────────

func TestTenantGet_InvalidTenantID_400(t *testing.T) {
	h := &TenantHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bogus")
	h.Get(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestTenantPatch_MalformedBody_400(t *testing.T) {
	h := &TenantHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodPatch, "/", `{"broken`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.Patch(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTenantPatch_CrossTenant_403(t *testing.T) {
	h := &TenantHandler{}
	tenantA := uuid.New()
	c, w := buildCtx(http.MethodPatch, "/", `{"name":"New"}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String())
	h.Patch(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// Reference-touch on the top of the file to make sure gin's TestMode is set.
// middleware_test.go also does this via init().
var _ = require.NotNil
