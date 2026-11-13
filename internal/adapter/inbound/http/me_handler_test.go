// P-35 GET /api/v1/me/session-context — handler + routing: the caller comes
// from the gateway identity headers only, the response is P-35's snake_case
// shape, errors are RFC 9457 problems, and RequireActiveTenant guards it.
// Pure unit test through NewRouter with a fake SessionContextReader; no DB.
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sessionContextPath = "/api/v1/me/session-context"

type fakeSessionContextReader struct {
	sc                 *service.SessionContext
	err                error
	calls              int
	gotTenant, gotUser uuid.UUID
}

func (f *fakeSessionContextReader) Get(_ context.Context, tenantID, userID uuid.UUID) (*service.SessionContext, error) {
	f.calls++
	f.gotTenant, f.gotUser = tenantID, userID
	return f.sc, f.err
}

func meRouter(reader SessionContextReader, tenantStatus domain.SubscriptionStatus) *Router {
	return NewRouter(RouterConfig{
		GinConfig:  gincommon.Config{Domain: "iam", Environment: "test", ServiceName: "iam-org-membership"},
		TenantRepo: &stubTenantRepo{tenant: &domain.Tenant{Status: tenantStatus}},
		MeHandler:  NewMeHandler(reader),
	})
}

func meRequest(target string, tenantID, userID uuid.UUID) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, http.NoBody)
	req.Header.Set("x-user-id", userID.String())
	req.Header.Set("x-tenant-id", tenantID.String())
	req.Header.Set("x-tenant-roles", "member")
	return req
}

func TestSessionContext_ReturnsCallerContext(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	reader := &fakeSessionContextReader{sc: &service.SessionContext{
		TenantID:            tenantID,
		Roles:               []domain.TenantRoleCode{domain.RoleMember, domain.RoleTenantAdmin},
		Departments:         []domain.DeptMembershipView{{DepartmentID: deptID, Code: "legal", RoleLevel: domain.DeptReviewer}},
		Plan:                domain.PlanPro,
		FeatureFlags:        []string{"ai_drafting", "read_only"},
		ReadOnly:            true,
		MFAFreshnessSeconds: 300,
		Locale:              "en",
		Slug:                "acme",
	}}
	w := httptest.NewRecorder()
	meRouter(reader, domain.StatusCancelled).Handler().ServeHTTP(w, meRequest(sessionContextPath, tenantID, userID))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, tenantID, reader.gotTenant)
	assert.Equal(t, userID, reader.gotUser)
	assert.JSONEq(t, `{
		"tenant_id": "`+tenantID.String()+`",
		"roles": ["member", "tenant_admin"],
		"departments": [{"id": "`+deptID.String()+`", "level": "reviewer"}],
		"plan": "pro",
		"feature_flags": ["ai_drafting", "read_only"],
		"read_only": true,
		"mfa_freshness_seconds": 300,
		"locale": "en",
		"slug": "acme"
	}`, w.Body.String())
}

// Identity comes from the gateway headers only: a tenant_id/user_id in the
// query string is ignored.
func TestSessionContext_IgnoresQueryIdentity(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	reader := &fakeSessionContextReader{sc: &service.SessionContext{TenantID: tenantID}}
	target := sessionContextPath + "?tenant_id=" + uuid.NewString() + "&user_id=" + uuid.NewString()
	w := httptest.NewRecorder()
	meRouter(reader, domain.StatusActive).Handler().ServeHTTP(w, meRequest(target, tenantID, userID))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, tenantID, reader.gotTenant)
	assert.Equal(t, userID, reader.gotUser)
}

// Empty collections are [] on the wire, never null.
func TestSessionContext_EmptyCollectionsAreArrays(t *testing.T) {
	reader := &fakeSessionContextReader{sc: &service.SessionContext{}}
	w := httptest.NewRecorder()
	meRouter(reader, domain.StatusActive).Handler().ServeHTTP(w, meRequest(sessionContextPath, uuid.New(), uuid.New()))

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, []any{}, body["roles"])
	assert.Equal(t, []any{}, body["departments"])
	assert.Equal(t, []any{}, body["feature_flags"])
}

func TestSessionContext_NoIdentityHeaders_401(t *testing.T) {
	reader := &fakeSessionContextReader{}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, sessionContextPath, http.NoBody)
	w := httptest.NewRecorder()
	meRouter(reader, domain.StatusActive).Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Zero(t, reader.calls)
}

// The service's I-8 answer (no membership) becomes a 404 problem.
func TestSessionContext_NoMembership_404Problem(t *testing.T) {
	reader := &fakeSessionContextReader{err: domain.NewError(domain.ErrMemberNotFound, "no active membership in this tenant")}
	w := httptest.NewRecorder()
	meRouter(reader, domain.StatusActive).Handler().ServeHTTP(w, meRequest(sessionContextPath, uuid.New(), uuid.New()))

	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	assert.Contains(t, w.Header().Get("Content-Type"), "application/problem+json")
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "MEMBER_NOT_FOUND", body["errorCode"])
}

func TestSessionContext_SuspendedMember_403Problem(t *testing.T) {
	reader := &fakeSessionContextReader{err: domain.NewError(domain.ErrInsufficientRole, "suspended members cannot access tenant APIs")}
	w := httptest.NewRecorder()
	meRouter(reader, domain.StatusActive).Handler().ServeHTTP(w, meRequest(sessionContextPath, uuid.New(), uuid.New()))

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "INSUFFICIENT_ROLE", body["errorCode"])
}

// RequireActiveTenant guards /me like every tenant-facing route (TRIAL-4):
// a trial_expired tenant is refused before the service runs.
func TestSessionContext_TrialExpiredTenant_403BeforeService(t *testing.T) {
	reader := &fakeSessionContextReader{sc: &service.SessionContext{}}
	w := httptest.NewRecorder()
	meRouter(reader, domain.StatusTrialExpired).Handler().ServeHTTP(w, meRequest(sessionContextPath, uuid.New(), uuid.New()))

	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "TENANT_TRIAL_EXPIRED", body["errorCode"])
	assert.Zero(t, reader.calls)
}

func TestSessionContext_OnlyGET(t *testing.T) {
	reader := &fakeSessionContextReader{sc: &service.SessionContext{}}
	req := meRequest(sessionContextPath, uuid.New(), uuid.New())
	req.Method = http.MethodPost
	w := httptest.NewRecorder()
	meRouter(reader, domain.StatusActive).Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	assert.Zero(t, reader.calls)
}

// Without GUCBridgeMiddleware's request context (a wiring bug), the handler
// answers 401 rather than reading anyone's context.
func TestSessionContext_HandlerWithoutRequestContext_401(t *testing.T) {
	reader := &fakeSessionContextReader{sc: &service.SessionContext{}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequestWithContext(context.Background(), http.MethodGet, sessionContextPath, http.NoBody)
	NewMeHandler(reader).SessionContext(c)

	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Zero(t, reader.calls)
}

// A router built without MeHandler registers no /me route: 404, not a nil
// dereference recovered as 500.
func TestSessionContext_NoMeHandler_RouteAbsent(t *testing.T) {
	r := NewRouter(RouterConfig{
		GinConfig:  gincommon.Config{Domain: "iam", Environment: "test", ServiceName: "iam-org-membership"},
		TenantRepo: &stubTenantRepo{tenant: &domain.Tenant{Status: domain.StatusActive}},
	})
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, meRequest(sessionContextPath, uuid.New(), uuid.New()))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}
