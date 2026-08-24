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

// ACLHandler.List/Revoke query-validation tests — retired ADR-0007 Wave 3
// Phase 6, moved to iam-tender-acl's TAC-1/3. IDs never reused.

// DelegationHandler.Cancel query-validation tests — retired ADR-0008 v2,
// moved to the standalone Delegation Service's DLG-3. IDs never reused.

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
