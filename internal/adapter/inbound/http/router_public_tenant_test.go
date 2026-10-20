// RequirePathTenantMatch — the API-2 guard on tenant-facing
// /api/v1/tenants/:id/* routes: unit tests, the route-registration guard,
// and a check that regrouping the routes under /tenants/:id left every URL
// unchanged.
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const publicTenantPrefix = "/api/v1/tenants/:id"

// gateCountingTenantRepo / gateCountingMembershipRepo record whether the
// activity gates ran (they must not, on a mismatch).
type gateCountingTenantRepo struct {
	port.TenantRepositoryNoop
	calls atomic.Int32
}

func (r *gateCountingTenantRepo) FindByID(context.Context, uuid.UUID) (*domain.Tenant, error) {
	r.calls.Add(1)
	return nil, domain.NewError(domain.ErrTenantNotFound, "not used")
}

type gateCountingMembershipRepo struct {
	port.MembershipRepository
	calls atomic.Int32
}

func (r *gateCountingMembershipRepo) FindByUserID(context.Context, uuid.UUID, uuid.UUID) (*domain.TenantMembership, error) {
	r.calls.Add(1)
	return nil, domain.NewError(domain.ErrMemberNotFound, "not used")
}

func publicGuardRouter(tr port.TenantRepository, mr port.MembershipRepository) *Router {
	return NewRouter(RouterConfig{
		GinConfig:      gincommon.Config{Domain: "iam", Environment: "test", ServiceName: "iam-org-membership"},
		TenantRepo:     tr,
		MembershipRepo: mr,
	})
}

func tenantOwnerRequest(method, path string, headerTenant uuid.UUID) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader("{}"))
	req.Header.Set("x-user-id", uuid.New().String())
	req.Header.Set("x-tenant-id", headerTenant.String())
	req.Header.Set("x-tenant-roles", "tenant_owner")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func publicTenantRoutes(r *Router) []string {
	var out []string
	for _, ri := range r.engine.Routes() {
		if ri.Path == publicTenantPrefix || strings.HasPrefix(ri.Path, publicTenantPrefix+"/") {
			out = append(out, ri.Method+" "+ri.Path)
		}
	}
	sort.Strings(out)
	return out
}

// Every registered /api/v1/tenants/:id/* route must reject a mismatched path
// tenant through RequirePathTenantMatch — identified by its own detail, since
// a handler's own check returns the same 403 INSUFFICIENT_ROLE — and before
// the activity gates query anything.
func TestPublicTenantRoutes_AllRequirePathTenantMatch(t *testing.T) {
	tr, mr := &gateCountingTenantRepo{}, &gateCountingMembershipRepo{}
	r := publicGuardRouter(tr, mr)
	routes := publicTenantRoutes(r)
	require.Len(t, routes, 21, "P-1..13, 24-28, 30-31, 34: %v", routes)

	for _, route := range routes {
		method, tmpl, _ := strings.Cut(route, " ")
		t.Run(route, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.Handler().ServeHTTP(w, tenantOwnerRequest(method, concretePath(tmpl, uuid.New()), uuid.New()))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			var body map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, "INSUFFICIENT_ROLE", body["errorCode"], "status/code unchanged from the handlers' own check")
			assert.Equal(t, PublicTenantMismatchDetail, body["detail"],
				"%s must be registered under the RequirePathTenantMatch group", route)
		})
	}
	assert.Zero(t, tr.calls.Load(), "activity gates must not query on a mismatch")
	assert.Zero(t, mr.calls.Load(), "activity gates must not query on a mismatch")
}

// Regrouping under /tenants/:id must not change a single URL.
func TestPublicTenantRoutes_PathsUnchanged(t *testing.T) {
	want := []string{
		"DELETE /api/v1/tenants/:id/departments/:dept_id/members/:user_id",
		"DELETE /api/v1/tenants/:id/invitations/:invitation_id",
		"DELETE /api/v1/tenants/:id/members/:user_id",
		"GET /api/v1/tenants/:id",
		"GET /api/v1/tenants/:id/departments",
		"GET /api/v1/tenants/:id/departments/:dept_id/members",
		"GET /api/v1/tenants/:id/invitations",
		"GET /api/v1/tenants/:id/members",
		"GET /api/v1/tenants/:id/members/:user_id",
		"GET /api/v1/tenants/:id/roles",
		"GET /api/v1/tenants/:id/seat-usage",
		"PATCH /api/v1/tenants/:id",
		"PATCH /api/v1/tenants/:id/departments/:dept_id",
		"PATCH /api/v1/tenants/:id/members/:user_id",
		"PATCH /api/v1/tenants/:id/roles/:role_code",
		"POST /api/v1/tenants/:id/departments",
		"POST /api/v1/tenants/:id/members",
		"POST /api/v1/tenants/:id/members/:user_id/reset-mfa",
		"POST /api/v1/tenants/:id/users/:user_id/removal-resolution",
		"PUT /api/v1/tenants/:id/departments/:dept_id/members/:user_id",
		"PUT /api/v1/tenants/:id/members/:user_id/roles",
	}
	assert.Equal(t, want, publicTenantRoutes(publicGuardRouter(nil, nil)))
}

// ── middleware unit tests ────────────────────────────────────────────────

func servePublicGuard(t *testing.T, pathID string, rc *requestctx.RequestContext) (*httptest.ResponseRecorder, *bool, *string) {
	t.Helper()
	called, gucTID := false, ""
	r := gin.New()
	r.GET(publicTenantPrefix,
		func(c *gin.Context) {
			if rc != nil {
				ctx := requestctx.WithContext(c.Request.Context(), rc)
				g, _ := pgcommon.GUCSetFromContext(ctx)
				g.TenantID = rc.TenantID.String()
				c.Request = c.Request.WithContext(pgcommon.WithGUCSet(ctx, g))
			}
			c.Next()
		},
		RequirePathTenantMatch(),
		func(c *gin.Context) {
			called = true
			if g, ok := pgcommon.GUCSetFromContext(c.Request.Context()); ok {
				gucTID = g.TenantID
			}
			c.Status(http.StatusOK)
		})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/tenants/"+pathID, nil))
	return w, &called, &gucTID
}

func TestRequirePathTenantMatch_Match_PassesWithoutRebinding(t *testing.T) {
	tenant := uuid.New()
	w, called, guc := servePublicGuard(t, tenant.String(), &requestctx.RequestContext{TenantID: tenant, Roles: []string{"tenant_owner"}})
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, *called)
	assert.Equal(t, tenant.String(), *guc, "the header tenant stays the RLS tenant")
}

func TestRequirePathTenantMatch_Mismatch_403NoHandler(t *testing.T) {
	headerTenant := uuid.New()
	w, called, _ := servePublicGuard(t, uuid.New().String(), &requestctx.RequestContext{TenantID: headerTenant, Roles: []string{"tenant_owner"}})
	assertErrorCode(t, w, http.StatusForbidden, "INSUFFICIENT_ROLE")
	assert.False(t, *called, "handler must not run on a mismatch")
}

// Operators and iam-system get no exemption: no public handler ever allowed a
// cross-tenant path, so neither does the guard.
func TestRequirePathTenantMatch_Mismatch_NoPrincipalExemption(t *testing.T) {
	for _, role := range []string{"platform_operator", "iam-system", "tenant_admin"} {
		w, called, _ := servePublicGuard(t, uuid.New().String(), &requestctx.RequestContext{TenantID: uuid.New(), Roles: []string{role}})
		assert.Equal(t, http.StatusForbidden, w.Code, role)
		assert.False(t, *called, role)
	}
}

func TestRequirePathTenantMatch_InvalidTenantID_400(t *testing.T) {
	w, called, _ := servePublicGuard(t, "not-a-uuid", &requestctx.RequestContext{TenantID: uuid.New()})
	assertErrorCode(t, w, http.StatusBadRequest, "VALIDATION_ERROR")
	assert.False(t, *called)
}

func TestRequirePathTenantMatch_NoRequestContext_401(t *testing.T) {
	w, called, _ := servePublicGuard(t, uuid.New().String(), nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.False(t, *called)
}
