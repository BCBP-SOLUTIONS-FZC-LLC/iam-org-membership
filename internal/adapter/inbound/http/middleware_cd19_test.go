package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// handlerChain runs middleware + a terminal handler that sets 200, returning
// the response recorder. The terminal handler is only reached if not aborted.
func handlerChain(middlewares ...gin.HandlerFunc) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", strings.NewReader(""))
	c.Request = req
	for _, mw := range middlewares {
		if c.IsAborted() {
			break
		}
		mw(c)
	}
	if !c.IsAborted() {
		c.Status(http.StatusOK)
	}
	return w
}

// withRC returns a middleware that injects rc into the request context.
func withRC(rc *requestctx.RequestContext) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := requestctx.WithContext(c.Request.Context(), rc)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// ── parseBridgedIdentity ──────────────────────────────────────────────────

func TestParseBridgedIdentity_CommercialSystem_MapsToSentinelUUID(t *testing.T) {
	rc, er := parseBridgedIdentity(bridgedIdentity{
		UserIDStr:   "commercial-system",
		TenantIDStr: "550e8400-e29b-41d4-a716-446655440000",
		Roles:       []string{"commercial-system"},
	})
	if er != nil {
		t.Fatalf("unexpected error: %+v", er)
	}
	assert.Equal(t, requestctx.CommercialSystemUUID, rc.UserID,
		"commercial-system must map to the fixed sentinel UUID")
	assert.True(t, rc.IsCommercialSystem())
	assert.False(t, rc.IsSystem(), "commercial-system must NOT be treated as iam-system")
}

func TestParseBridgedIdentity_IamSystem_StillMapsToNil(t *testing.T) {
	rc, er := parseBridgedIdentity(bridgedIdentity{
		UserIDStr:   "iam-system",
		TenantIDStr: "550e8400-e29b-41d4-a716-446655440000",
		Roles:       []string{"iam-system"},
	})
	if er != nil {
		t.Fatalf("unexpected error: %+v", er)
	}
	// uuid.Nil is the iam-system sentinel — behaviour unchanged after CD-19.
	assert.True(t, rc.UserID.String() == "00000000-0000-0000-0000-000000000000")
	assert.True(t, rc.IsSystem())
}

// ── RequireInternalCaller — iam-system ────────────────────────────────────

func TestRequireInternalCaller_IamSystem_AllRoutes_Pass(t *testing.T) {
	routes := []string{
		"/api/v1/internal/tenants/:id/seat-usage",
		"/api/v1/internal/tenants/:id/plan-context",
		"/api/v1/internal/tenants/:id/plan-change-check",
		"/api/v1/internal/tenants/:id",           // I-2
		"/api/v1/internal/users/:id/memberships", // I-8
		"/api/v1/internal/tenants",               // I-1
	}
	rc := &requestctx.RequestContext{Roles: []string{"iam-system"}}
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			w := handlerChain(withRC(rc), func(c *gin.Context) {
				// Simulate Gin setting FullPath for this route.
				c.Params = gin.Params{}
				// We can't override c.FullPath() directly in tests, but since
				// iam-system bypasses the allow-list entirely the route doesn't matter.
				RequireInternalCaller()(c)
			})
			assert.Equal(t, http.StatusOK, w.Code,
				"iam-system must pass on %s", route)
		})
	}
}

// ── RequireInternalCaller — commercial-system ─────────────────────────────

// simulateRoute injects a fake FullPath via a real Gin engine so
// c.FullPath() returns the expected pattern inside RequireInternalCaller.
func simulateRoute(t *testing.T, method, fullPath string, rc *requestctx.RequestContext) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := gin.New()
	r.Handle(method, fullPath, withRC(rc), RequireInternalCaller(), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequestWithContext(context.Background(), method, strings.ReplaceAll(fullPath, ":id", "550e8400-e29b-41d4-a716-446655440000"), nil)
	r.ServeHTTP(w, req)
	return w
}

func TestRequireInternalCaller_CommercialSystem_I11_Allowed(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"commercial-system"}}
	w := simulateRoute(t, "GET", "/api/v1/internal/tenants/:id/seat-usage", rc)
	assert.Equal(t, http.StatusOK, w.Code, "commercial-system must be allowed on I-11")
}

func TestRequireInternalCaller_CommercialSystem_I17_Allowed(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"commercial-system"}}
	w := simulateRoute(t, "GET", "/api/v1/internal/tenants/:id/plan-context", rc)
	assert.Equal(t, http.StatusOK, w.Code, "commercial-system must be allowed on I-17")
}

func TestRequireInternalCaller_CommercialSystem_I18_Allowed(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"commercial-system"}}
	w := simulateRoute(t, "GET", "/api/v1/internal/tenants/:id/plan-change-check", rc)
	assert.Equal(t, http.StatusOK, w.Code, "commercial-system must be allowed on I-18")
}

func TestRequireInternalCaller_CommercialSystem_I1_Forbidden(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"commercial-system"}}
	w := simulateRoute(t, "POST", "/api/v1/internal/tenants", rc)
	assert.Equal(t, http.StatusForbidden, w.Code,
		"commercial-system must be 403 on I-1 (not in allow-list)")
}

func TestRequireInternalCaller_CommercialSystem_I8_Forbidden(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"commercial-system"}}
	w := simulateRoute(t, "GET", "/api/v1/internal/users/:id/memberships", rc)
	assert.Equal(t, http.StatusForbidden, w.Code,
		"commercial-system must be 403 on I-8 (not in allow-list)")
}

func TestRequireInternalCaller_CommercialSystem_I16_Forbidden(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"commercial-system"}}
	w := simulateRoute(t, "GET", "/api/v1/internal/subscription-lapses", rc)
	assert.Equal(t, http.StatusForbidden, w.Code,
		"commercial-system must be 403 on I-16 (not in allow-list)")
}

func TestRequireInternalCaller_CommercialSystem_MemberWrite_Forbidden(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"commercial-system"}}
	w := simulateRoute(t, "POST", "/api/v1/internal/tenants/:id/members", rc)
	assert.Equal(t, http.StatusForbidden, w.Code,
		"commercial-system must be 403 on membership writes")
}

// ── RequireInternalCaller — regular users ────────────────────────────────

func TestRequireInternalCaller_RegularUser_Forbidden(t *testing.T) {
	rc := &requestctx.RequestContext{Roles: []string{"tenant_admin"}}
	w := simulateRoute(t, "GET", "/api/v1/internal/tenants/:id/seat-usage", rc)
	assert.Equal(t, http.StatusForbidden, w.Code,
		"regular tenant roles must be 403 on internal routes")
}

func TestRequireInternalCaller_NoContext_Forbidden(t *testing.T) {
	w := handlerChain(RequireInternalCaller())
	assert.Equal(t, http.StatusForbidden, w.Code,
		"missing request context must result in 403")
}

// ── commercialSystemAllowedRoutes — stable-key guard ─────────────────────

// TestCommercialSystemAllowedRoutes_StableKeys asserts the exact key strings
// in the allow-list map. A key mismatch (e.g. from a route rename) would
// silently block commercial-system without a failing test.
func TestCommercialSystemAllowedRoutes_StableKeys(t *testing.T) {
	want := []string{
		"GET /api/v1/internal/tenants/:id/seat-usage",
		"GET /api/v1/internal/tenants/:id/plan-context",
		"GET /api/v1/internal/tenants/:id/plan-change-check",
	}
	for _, k := range want {
		assert.True(t, commercialSystemAllowedRoutes[k],
			"allow-list must contain key %q — a route rename breaks CD-19", k)
	}
	assert.Len(t, commercialSystemAllowedRoutes, len(want),
		"allow-list must have exactly %d entries — add a test if you add a route", len(want))
}
