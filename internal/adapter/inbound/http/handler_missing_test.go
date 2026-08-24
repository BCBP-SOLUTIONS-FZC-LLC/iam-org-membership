package http

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Handler methods that had no matrix coverage: their nil-service early-return
// branches (invalid UUID, missing identity, cross-tenant, body validation)
// are still worth locking so future refactors can't silently degrade the
// error taxonomy. Mirrors the design invariant in handler_matrix_test.go:
// nil service — if input validation leaks through, the test nil-panics.

// DelegationHandler.List (P-18) tests retired — moved to the standalone
// Delegation Service's DLG-1 (ADR-0008 v2). ID never reused.

// ── MembershipHandler.Patch (P-7) — early-return matrix ───────────────

func TestMembershipPatch_InvalidTenantID(t *testing.T) {
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", "not-a-uuid", "user_id", uuid.New().String())
	h.Patch(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestMembershipPatch_InvalidUserID(t *testing.T) {
	tenant := uuid.New()
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", "bogus")
	h.Patch(c)
	assertErrorCode(t, w, http.StatusBadRequest, "invalid_uuid")
}

func TestMembershipPatch_CrossTenant(t *testing.T) {
	// Caller in tenant B targets tenant A → cross-tenant blocked.
	tenantA := uuid.New()
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{}`, tenantOwnerCtx(uuid.New()))
	setParams(c, "id", tenantA.String(), "user_id", uuid.New().String())
	h.Patch(c)
	assertErrorCode(t, w, http.StatusForbidden, "insufficient_role")
}

func TestMembershipPatch_MalformedJSON(t *testing.T) {
	tenant := uuid.New()
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", uuid.New().String())
	h.Patch(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

func TestMembershipPatch_MissingStatus(t *testing.T) {
	tenant := uuid.New()
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", uuid.New().String())
	h.Patch(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}

func TestMembershipPatch_InvalidStatus(t *testing.T) {
	tenant := uuid.New()
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodPatch, "/",
		`{"status":"deleted","record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "user_id", uuid.New().String())
	h.Patch(c)
	assertErrorCode(t, w, http.StatusBadRequest, "validation_error")
}
