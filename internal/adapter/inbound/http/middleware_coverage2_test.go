// Coverage tests closing the remaining middleware.go gaps from the prior
// session: RequireActiveMembership (near-zero coverage — the biggest single
// gap), the GUCBridgeMiddleware malformed-identity error branch (driven
// through the real router + gincommon.ProtectedMiddlewares, per the
// TestRouter_I16_* pattern in router_i16_crosstenant_test.go — the
// gincommon RequestContext type is internal to that module and cannot be
// constructed directly), RequireJSONContentType's POST-with-empty-body
// branch, and RequireActiveTenant's ErrTenantNotFound lookup branch.
package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// ─────────────────────────────────────────────────────────────────────────
// RequireActiveMembership — was 4.3%, the biggest single gap.
// ─────────────────────────────────────────────────────────────────────────

func TestRequireActiveMembership_MemberNotFound_Returns403(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	rc := &requestctx.RequestContext{TenantID: tenantID, UserID: userID, Roles: []string{"tenant_admin"}}
	c, w := newTestContext(rc)

	repo := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, domain.ErrMemberNotFound
	}}
	RequireActiveMembership(repo)(c)

	assert.True(t, c.IsAborted())
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "FORBIDDEN", decodeBody(t, w)["errorCode"])
}

func TestRequireActiveMembership_RepoError_PassesThroughToHandler(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	rc := &requestctx.RequestContext{TenantID: tenantID, UserID: userID, Roles: []string{"tenant_admin"}}
	c, w := newTestContext(rc)
	called := false
	handler := gin.HandlerFunc(func(c *gin.Context) { called = true; c.Status(http.StatusOK) })

	repo := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		return nil, errors.New("db down")
	}}
	RequireActiveMembership(repo)(c)
	if !c.IsAborted() {
		handler(c)
	}
	assert.True(t, called, "a real DB error must fall through, not be swallowed as forbidden")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireActiveMembership_SuspendedMember_Returns403(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	rc := &requestctx.RequestContext{TenantID: tenantID, UserID: userID, Roles: []string{"tenant_admin"}}
	c, w := newTestContext(rc)

	repo := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipSuspended}, nil
	}}
	RequireActiveMembership(repo)(c)

	assert.True(t, c.IsAborted())
	assert.Equal(t, http.StatusForbidden, w.Code)
	body := decodeBody(t, w)
	assert.Equal(t, "FORBIDDEN", body["errorCode"])
}

func TestRequireActiveMembership_ActiveMember_Proceeds(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	rc := &requestctx.RequestContext{TenantID: tenantID, UserID: userID, Roles: []string{"tenant_admin"}}
	c, w := newTestContext(rc)
	called := false
	handler := gin.HandlerFunc(func(c *gin.Context) { called = true; c.Status(http.StatusOK) })

	repo := &mhMemRepo{findByUserFn: func(_ context.Context, tid, uid uuid.UUID) (*domain.TenantMembership, error) {
		return &domain.TenantMembership{ID: uuid.New(), TenantID: tid, UserID: uid, Status: domain.MembershipActive}, nil
	}}
	RequireActiveMembership(repo)(c)
	if !c.IsAborted() {
		handler(c)
	}
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ─────────────────────────────────────────────────────────────────────────
// GUCBridgeMiddleware — malformed-identity error branch (lines 97-103).
// Driven through the real router: RequireAuth/ContextMiddleware only
// validate header PRESENCE, not UUID format, so a non-UUID x-user-id
// reaches GUCBridgeMiddleware itself, which fails to parse it.
// ─────────────────────────────────────────────────────────────────────────

func TestGUCBridgeMiddleware_MalformedUserIDHeader_Returns401ViaRouter(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{Domain: "iam", Environment: "test", ServiceName: "iam-org-membership"},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/v1/tenants/"+uuid.New().String(), http.NoBody)
	req.Header.Set("x-user-id", "not-a-uuid")
	req.Header.Set("x-tenant-id", uuid.New().String())
	w := httptest.NewRecorder()

	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "UNAUTHORIZED")
}

func TestGUCBridgeMiddleware_MalformedTenantIDHeader_Returns401ViaRouter(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{Domain: "iam", Environment: "test", ServiceName: "iam-org-membership"},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/v1/tenants/"+uuid.New().String(), http.NoBody)
	req.Header.Set("x-user-id", uuid.New().String())
	req.Header.Set("x-tenant-id", "not-a-uuid")
	w := httptest.NewRecorder()

	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "UNAUTHORIZED")
}

// ─────────────────────────────────────────────────────────────────────────
// RequireJSONContentType — POST with ContentLength==0 (the switch case
// falls into this branch only for POST/PUT/PATCH; the existing
// TestRequireJSONContentType_SkipsEmptyBody test uses DELETE, which never
// reaches this line since it's caught by the method switch's default arm).
// ─────────────────────────────────────────────────────────────────────────

func TestRequireJSONContentType_SkipsEmptyBodyOnPOST(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil)
	c.Request.ContentLength = 0

	RequireJSONContentType()(c)

	assert.False(t, c.IsAborted())
}

// ─────────────────────────────────────────────────────────────────────────
// RequireActiveTenant — ErrTenantNotFound lookup branch (distinct from the
// generic-error-falls-through branch already covered in
// require_active_tenant_test.go).
// ─────────────────────────────────────────────────────────────────────────

func TestRequireActiveTenant_LookupErrorTenantNotFound_Returns404(t *testing.T) {
	repo := &stubTenantRepo{err: domain.ErrTenantNotFound}
	mw := RequireActiveTenant(repo)

	rc := &requestctx.RequestContext{TenantID: uuid.New(), Roles: []string{"tenant_owner"}}
	c, w := newTestContext(rc)

	mw(c)

	assert.True(t, c.IsAborted())
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "TENANT_NOT_FOUND", decodeBody(t, w)["errorCode"])
}
