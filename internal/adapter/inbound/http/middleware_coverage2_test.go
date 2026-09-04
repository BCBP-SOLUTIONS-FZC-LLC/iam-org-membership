// Coverage tests closing the remaining middleware.go gaps from the prior
// session: RequireActiveMembership (near-zero coverage — the biggest single
// gap), the GUCBridgeMiddleware malformed-identity error branch (driven
// through the real router + gincommon.ProtectedMiddlewares, per the
// TestRouter_I16_* pattern in router_i16_crosstenant_test.go — the
// gincommon RequestContext type is internal to that module and cannot be
// constructed directly), RequireJSONContentType's POST-with-empty-body
// branch, the bufferedWriter helper methods, HandleError's errorLogger
// call site, and RequireActiveTenant's ErrTenantNotFound lookup branch.
package http

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────
// RequireActiveMembership — was 4.3%, the biggest single gap.
// ─────────────────────────────────────────────────────────────────────────

func TestRequireActiveMembership_NoIdentity_PassesThrough(t *testing.T) {
	c, w := newTestContext(nil)
	called := false
	handler := gin.HandlerFunc(func(c *gin.Context) { called = true; c.Status(http.StatusOK) })

	RequireActiveMembership(&mhMemRepo{})(c)
	if !c.IsAborted() {
		handler(c)
	}
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireActiveMembership_SystemPrincipal_Bypasses(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"iam-system"}}
	c, _ := newTestContext(rc)
	called := false
	handler := gin.HandlerFunc(func(c *gin.Context) { called = true; c.Status(http.StatusOK) })

	// A repo that would panic-via-error if actually consulted, to prove the
	// bypass short-circuits before any lookup.
	repo := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		t.Fatal("iam-system must bypass the membership lookup entirely")
		return nil, nil
	}}
	RequireActiveMembership(repo)(c)
	if !c.IsAborted() {
		handler(c)
	}
	assert.True(t, called)
}

func TestRequireActiveMembership_Operator_Bypasses(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"platform_operator"}}
	c, _ := newTestContext(rc)
	called := false
	handler := gin.HandlerFunc(func(c *gin.Context) { called = true; c.Status(http.StatusOK) })

	repo := &mhMemRepo{findByUserFn: func(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
		t.Fatal("platform_operator must bypass the membership lookup entirely")
		return nil, nil
	}}
	RequireActiveMembership(repo)(c)
	if !c.IsAborted() {
		handler(c)
	}
	assert.True(t, called)
}

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
	assert.Equal(t, "insufficient_role", decodeBody(t, w)["code"])
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
	assert.Equal(t, "insufficient_role", body["code"])
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
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/v1/tenants/"+uuid.New().String(), http.NoBody)
	req.Header.Set("x-user-id", "not-a-uuid")
	req.Header.Set("x-tenant-id", uuid.New().String())
	w := httptest.NewRecorder()

	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "missing_identity_headers")
}

func TestGUCBridgeMiddleware_MalformedTenantIDHeader_Returns401ViaRouter(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/v1/tenants/"+uuid.New().String(), http.NoBody)
	req.Header.Set("x-user-id", uuid.New().String())
	req.Header.Set("x-tenant-id", "not-a-uuid")
	w := httptest.NewRecorder()

	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "missing_identity_headers")
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
// bufferedWriter — Write / Status / Written / WriteString direct coverage.
// ─────────────────────────────────────────────────────────────────────────

// newBufferedWriterForTest builds a bufferedWriter wrapping a real
// gin.ResponseWriter (httptest.ResponseRecorder alone doesn't implement the
// gin.ResponseWriter interface — it's missing CloseNotify etc.).
func newBufferedWriterForTest() *bufferedWriter {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	return &bufferedWriter{ResponseWriter: c.Writer, buf: new(bytes.Buffer)}
}

func TestBufferedWriter_WriteSetsStatusOKWhenUnset(t *testing.T) {
	bw := newBufferedWriterForTest()

	n, err := bw.Write([]byte("hello"))
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.Equal(t, http.StatusOK, bw.status)
}

func TestBufferedWriter_StatusFallsBackToWrappedWriterWhenUnset(t *testing.T) {
	bw := newBufferedWriterForTest()

	// status field is zero — Status() must delegate to the wrapped
	// gin.ResponseWriter (defaults to 200 on a fresh writer).
	assert.Equal(t, bw.ResponseWriter.Status(), bw.Status())
}

func TestBufferedWriter_StatusReturnsBufferedValueWhenSet(t *testing.T) {
	bw := newBufferedWriterForTest()
	bw.WriteHeader(http.StatusTeapot)

	assert.Equal(t, http.StatusTeapot, bw.Status())
}

func TestBufferedWriter_WrittenReflectsBufferedState(t *testing.T) {
	bw := newBufferedWriterForTest()
	assert.False(t, bw.Written())

	_, _ = bw.Write([]byte("x"))
	assert.True(t, bw.Written())
}

func TestBufferedWriter_WriteString(t *testing.T) {
	bw := newBufferedWriterForTest()

	n, err := bw.WriteString("hi")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, "hi", bw.buf.String())
}

// ─────────────────────────────────────────────────────────────────────────
// HandleError — errorLogger != nil call site on the unhandled-500 branch.
// ─────────────────────────────────────────────────────────────────────────

func TestHandleError_LogsThroughErrorLoggerWhenSet(t *testing.T) {
	orig := errorLogger
	logger := &rcTestLogger{}
	errorLogger = logger
	defer func() { errorLogger = orig }()

	c, w := newTestContext(nil)
	HandleError(c, errors.New("boom, unhandled"))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.True(t, logger.errorCalled, "unhandled 500s must log through errorLogger when set")
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
	assert.Equal(t, "tenant_not_found", decodeBody(t, w)["code"])
}
