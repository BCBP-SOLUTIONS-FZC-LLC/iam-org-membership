package http

// Handler/middleware tests for the code-vs-LLD fixes of 2026-11-11 (LLD rev
// 2.52): AUTH-5 system principal on tenant-facing routes (BUG-7), §17
// ambiguous_identity_headers (GAP-25), invalid_role / invalid_role_level 400
// (OBS-2 / OBS-8), O-4 record_version required (OBS-7), P-4 limit (GAP-3).

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func runPrincipalGate(t *testing.T, gate gin.HandlerFunc, rc *requestctx.RequestContext) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	c, w := buildCtx(http.MethodGet, "/", "", rc)
	reached := false
	gate(c)
	if !c.IsAborted() {
		reached = true
	}
	return w, reached
}

// BUG-7 / AUTH-5: the system principal is refused on tenant-facing routes.
func TestRejectSystemPrincipal_SystemRole_403(t *testing.T) {
	w, reached := runPrincipalGate(t, RejectSystemPrincipal(), iamSystemCtx(uuid.New()))
	assert.False(t, reached, "iam-system must not reach a tenant-facing handler")
	assertErrorCode(t, w, http.StatusForbidden, "INSUFFICIENT_ROLE")
}

// x-user-id "iam-system" is bridged to uuid.Nil whatever roles it carries.
func TestRejectSystemPrincipal_SystemUserWithOwnerRole_403(t *testing.T) {
	rc := &requestctx.RequestContext{UserID: uuid.Nil, TenantID: uuid.New(), Roles: []string{"tenant_owner"}}
	w, reached := runPrincipalGate(t, RejectSystemPrincipal(), rc)
	assert.False(t, reached)
	assertErrorCode(t, w, http.StatusForbidden, "INSUFFICIENT_ROLE")
}

func TestRejectSystemPrincipal_SentinelUUID_403(t *testing.T) {
	rc := &requestctx.RequestContext{UserID: uuid.MustParse(requestctx.IamSystemSentinelUUID), TenantID: uuid.New()}
	_, reached := runPrincipalGate(t, RejectSystemPrincipal(), rc)
	assert.False(t, reached)
}

func TestRejectSystemPrincipal_TenantOwner_Passes(t *testing.T) {
	_, reached := runPrincipalGate(t, RejectSystemPrincipal(), tenantOwnerCtx(uuid.New()))
	assert.True(t, reached, "an ordinary member is not affected")
}

// GAP-25 / LLD §17: a repeated identity header → 401 ambiguous_identity_headers.
func TestRejectAmbiguousIdentityHeaders_RepeatedHeader_401(t *testing.T) {
	for _, h := range []string{"x-user-id", "x-tenant-id"} {
		t.Run(h, func(t *testing.T) {
			c, w := buildCtx(http.MethodGet, "/", "", nil)
			c.Request.Header.Add(h, uuid.New().String())
			c.Request.Header.Add(h, uuid.New().String())
			RejectAmbiguousIdentityHeaders()(c)
			assert.True(t, c.IsAborted())
			assertErrorCode(t, w, http.StatusUnauthorized, "AMBIGUOUS_IDENTITY_HEADERS")
		})
	}
}

func TestRejectAmbiguousIdentityHeaders_SingleHeaders_Pass(t *testing.T) {
	c, _ := buildCtx(http.MethodGet, "/", "", nil)
	c.Request.Header.Set("x-user-id", uuid.New().String())
	c.Request.Header.Set("x-tenant-id", uuid.New().String())
	RejectAmbiguousIdentityHeaders()(c)
	assert.False(t, c.IsAborted())
}

// OBS-2 / OBS-8, LLD §17: invalid_role → 400, P-10 level → 400 invalid_role_level.
func TestDomainError_InvalidRole_400(t *testing.T) {
	c, w := buildCtx(http.MethodGet, "/", "", nil)
	HandleError(c, domain.NewError(domain.ErrInvalidRole, "unknown role_code").
		WithDetails(map[string]any{"code": "invalid_role"}))
	assertErrorCode(t, w, http.StatusBadRequest, "INVALID_ROLE")
}

func TestDomainError_InvalidRoleLevel_400(t *testing.T) {
	c, w := buildCtx(http.MethodGet, "/", "", nil)
	HandleError(c, domain.NewError(domain.ErrInvalidRole, "invalid role_level").
		WithDetails(map[string]any{"code": "invalid_role_level"}))
	assertErrorCode(t, w, http.StatusBadRequest, "INVALID_ROLE_LEVEL")
}

// OBS-7: O-4 without record_version is 400, not a 409 version conflict.
func TestSetFeatureFlags_MissingRecordVersion_400(t *testing.T) {
	h := NewOperatorHandler(nil)
	c, w := buildCtx(http.MethodPatch, "/", `{}`, operatorCtx())
	setParams(c, "id", uuid.New().String())
	h.SetFeatureFlags(c)
	assertErrorCode(t, w, http.StatusBadRequest, "VALIDATION_ERROR")
}

// GAP-3 / PAGE-4: P-4 limit must be in [1, 100].
func TestMembershipList_LimitAbove100_400(t *testing.T) {
	tenant := uuid.New()
	h := &MembershipHandler{}
	c, w := buildCtx(http.MethodGet, "/?limit=101", "", tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String())
	h.List(c)
	assertErrorCode(t, w, http.StatusBadRequest, "VALIDATION_ERROR")
}

// GAP-25: P-13 with only record_version → 400 no_mutable_field.
func TestRoleLabelPatch_NoDisplayName_NoMutableField(t *testing.T) {
	tenant := uuid.New()
	h := &RoleLabelHandler{}
	c, w := buildCtx(http.MethodPatch, "/", `{"record_version":1}`, tenantOwnerCtx(tenant))
	setParams(c, "id", tenant.String(), "role_code", "preparator")
	h.Patch(c)
	assertErrorCode(t, w, http.StatusBadRequest, "NO_MUTABLE_FIELD")
}
