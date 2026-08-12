package http

import (
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Handlers accept optional query params (limit, cursor) and body fields.
// Their input-validation branches are usually not exercised by the
// happy-path tests in the postgres suite because those tests use default
// params. This file locks the malformed-input paths.

// ── MembershipHandler.List — limit + cursor query validation ────────────

func TestP4List_InvalidLimit(t *testing.T) {
	h := &MembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodGet, "/?limit=notanumber", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_limit")
}

func TestP4List_LimitZero(t *testing.T) {
	h := &MembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodGet, "/?limit=0", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_limit")
}

func TestP4List_LimitTooHigh(t *testing.T) {
	h := &MembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodGet, "/?limit=500", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_limit")
}

func TestP4List_InvalidCursorBase64(t *testing.T) {
	h := &MembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodGet, "/?cursor=!!!not-b64!!!", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_cursor")
}

func TestP4List_InvalidCursorJSON(t *testing.T) {
	// Valid base64 that decodes to garbage JSON.
	badJSON := base64.URLEncoding.EncodeToString([]byte("{not json"))
	h := &MembershipHandler{}
	tenant := uuid.New()
	c, w := buildCtx(http.MethodGet, "/?cursor="+badJSON, ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_cursor")
}

// ── MembershipHandler.Get — user_id param validation ────────────────────

func TestP5Get_InvalidTenantID(t *testing.T) {
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid", "user_id", uuid.New().String())
	h.Get(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestP5Get_InvalidUserID(t *testing.T) {
	tenant := uuid.New()
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", "not-a-uuid")
	h.Get(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestP5Get_CrossTenant(t *testing.T) {
	tenantA := uuid.New()
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String(), "user_id", uuid.New().String())
	h.Get(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

// ── ACLHandler.List — tender_id + tenant_id validation ─────────────────

func TestP21ACLList_InvalidTenantID(t *testing.T) {
	h := &ACLHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bad", "tender_id", uuid.New().String())
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestP21ACLList_InvalidTenderID(t *testing.T) {
	tenant := uuid.New()
	h := &ACLHandler{}
	c, w := buildCtx(http.MethodGet, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "tender_id", "bad")
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── ACLHandler.Revoke — same shape ─────────────────────────────────────

func TestP23ACLRevoke_InvalidTenantID(t *testing.T) {
	h := &ACLHandler{}
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bad", "tender_id", uuid.New().String(), "user_id", uuid.New().String())
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestP23ACLRevoke_InvalidUserID(t *testing.T) {
	tenant := uuid.New()
	h := &ACLHandler{}
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "tender_id", uuid.New().String(), "user_id", "bogus")
	h.Revoke(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── DelegationHandler.Cancel — delegation-id path param + identity gate ─

func TestP20DelegCancel_InvalidDelegationID(t *testing.T) {
	h := &DelegationHandler{}
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bogus") // {id} in route is the delegation UUID
	h.Cancel(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestP20DelegCancel_MissingIdentity(t *testing.T) {
	h := &DelegationHandler{}
	c, w := buildCtx(http.MethodDelete, "/", ``, nil)
	setParams(c, "id", uuid.New().String())
	h.Cancel(c)
	assertErrorCode(t, w, http.StatusUnauthorized, "missing_identity_headers")
}

// ── DeptMembershipHandler.Remove — same validation shape ────────────────

func TestP11DeptRemove_InvalidTenantID(t *testing.T) {
	h := &DeptMembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "bad", "user_id", uuid.New().String(), "dept_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestP11DeptRemove_InvalidUserID(t *testing.T) {
	tenant := uuid.New()
	h := &DeptMembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", "bad", "dept_id", uuid.New().String())
	h.Remove(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestP11DeptRemove_InvalidDeptID(t *testing.T) {
	tenant := uuid.New()
	h := &DeptMembershipHandler{}
	c, w := buildCtx(http.MethodDelete, "/", ``, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", uuid.New().String(), "dept_id", "bad")
	h.Remove(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

// ── OperatorHandler.PatchDepartment — bad UUID / missing role gates ─────

func TestOP2Patch_InvalidDeptID(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"name":"X","record_version":1}`,
		operatorCtx())
	setParams(c, "id", "not-a-uuid")
	h.PatchDepartment(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestOP2Patch_MissingBody(t *testing.T) {
	h := &OperatorHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{`,
		operatorCtx())
	setParams(c, "id", uuid.New().String())
	h.PatchDepartment(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}
