// Route-registration guard for BindPathTenant: every registered
// /api/v1/internal/tenants/:id/* route must sit behind it. The router is
// built in strict mode and each route is called with an x-tenant-id that
// differs from the path tenant: a covered route answers 400 TENANT_MISMATCH
// before its handler runs; a route registered outside the group reaches its
// (service-less) handler instead and answers something else, failing CI.
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const internalTenantPrefix = internalTenantRoutePrefix

func strictRouter(strict bool) *Router {
	return NewRouter(RouterConfig{
		GinConfig:                  gincommon.Config{Domain: "iam", Environment: "test", ServiceName: "iam-org-membership"},
		InternalHandler:            NewInternalHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil),
		StrictTenantPathValidation: strict,
	})
}

// concretePath fills every :param with a fresh UUID; :id gets pathTenant.
func concretePath(tmpl string, pathTenant uuid.UUID) string {
	parts := strings.Split(tmpl, "/")
	for i, p := range parts {
		switch {
		case p == ":id":
			parts[i] = pathTenant.String()
		case strings.HasPrefix(p, ":"):
			parts[i] = uuid.New().String()
		}
	}
	return strings.Join(parts, "/")
}

func iamSystemRequest(method, path string, headerTenant uuid.UUID) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader("{}"))
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-id", headerTenant.String())
	req.Header.Set("x-tenant-roles", "iam-system")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func tenantScopedRoutes(t *testing.T, r *Router) []string {
	t.Helper()
	var out []string
	for _, ri := range r.engine.Routes() {
		if ri.Path == internalTenantPrefix || strings.HasPrefix(ri.Path, internalTenantPrefix+"/") {
			out = append(out, ri.Method+" "+ri.Path)
		}
	}
	return out
}

func TestInternalTenantRoutes_AllBindPathTenant(t *testing.T) {
	r := strictRouter(true)
	routes := tenantScopedRoutes(t, r)
	// I-2, I-3, I-4, I-5, I-9, I-10, I-11, I-13, I-14, I-15, I-17, I-18, I-19, I-20.
	require.GreaterOrEqual(t, len(routes), 14, "expected every tenant-scoped internal route, got %v", routes)

	for _, route := range routes {
		method, tmpl, _ := strings.Cut(route, " ")
		t.Run(route, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.Handler().ServeHTTP(w, iamSystemRequest(method, concretePath(tmpl, uuid.New()), uuid.New()))
			require.Equal(t, http.StatusBadRequest, w.Code,
				"%s must be registered under the BindPathTenant group (body: %s)", route, w.Body.String())
			var body map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, "TENANT_MISMATCH", body["errorCode"], route)
		})
	}
}

// Routes whose :id is not a tenant stay outside the group: a mismatched
// header on I-8 (/users/:id) is not a tenant mismatch.
func TestInternalNonTenantRoutes_NotBound(t *testing.T) {
	r := strictRouter(true)
	w := httptest.NewRecorder()
	req := iamSystemRequest(http.MethodGet, "/api/v1/internal/users/"+uuid.New().String()+"/memberships?tenant_id=not-a-uuid", uuid.New())
	r.Handler().ServeHTTP(w, req)
	assert.NotEqual(t, "TENANT_MISMATCH", errorCodeOf(w), "I-8 must not be treated as tenant-scoped")
	assertErrorCode(t, w, http.StatusBadRequest, "VALIDATION_ERROR") // its own ?tenant_id= validation
}

// Strict off (the default): the same mismatched request is not rejected by
// the middleware.
func TestInternalTenantRoutes_NonStrict_NoRejection(t *testing.T) {
	r := strictRouter(false)
	w := httptest.NewRecorder()
	req := iamSystemRequest(http.MethodGet, "/api/v1/internal/tenants/"+uuid.New().String()+"/members/"+uuid.New().String()+"/eligibility?department_id=bad", uuid.New())
	r.Handler().ServeHTTP(w, req)
	assert.NotEqual(t, "TENANT_MISMATCH", errorCodeOf(w))
	assertErrorCode(t, w, http.StatusBadRequest, "VALIDATION_ERROR") // reached the handler's own validation
}

func errorCodeOf(w *httptest.ResponseRecorder) string {
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	s, _ := body["errorCode"].(string)
	return s
}
