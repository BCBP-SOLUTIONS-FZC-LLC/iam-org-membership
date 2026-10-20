// BindPathTenant unit tests: tenant-ID validation, path tenant bound as the
// RLS/RequestContext tenant with the caller identity preserved, mismatch
// override (non-strict) with warning log + metric, strict-mode 400
// TENANT_MISMATCH with no handler execution.
package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ptRoute = "/api/v1/internal/tenants/:id/locale"

var ptMetricsOnce sync.Once

// ptInitMetrics initialises gincommon's identity the way every router test
// in this package does (through NewRouter, so the build version matches
// whatever an earlier test already registered) and builds this service's
// collectors once, as cmd/server does, so the mismatch counter is non-nil.
func ptInitMetrics(t *testing.T) {
	t.Helper()
	ptMetricsOnce.Do(func() {
		_ = NewRouter(RouterConfig{
			GinConfig: gincommon.Config{Domain: "iam", Environment: "test", ServiceName: "iam-org-membership"},
		})
		metrics.Register("test")
	})
	require.NotNil(t, metrics.TenantHeaderPathMismatch)
}

// ptLogger records Warn calls.
type ptLogger struct {
	mu    sync.Mutex
	warns []map[string]interface{}
	msgs  []string
}

func (l *ptLogger) Debug(string, map[string]interface{}) {}
func (l *ptLogger) Info(string, map[string]interface{})  {}
func (l *ptLogger) Error(string, map[string]interface{}) {}
func (l *ptLogger) Warn(msg string, f map[string]interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, msg)
	l.warns = append(l.warns, f)
}

// seen is what the downstream handler observed.
type seen struct {
	called bool
	gucTID string
	gucUID string
	rc     requestctx.RequestContext
	hasRC  bool
}

// servePT runs BindPathTenant on ptRoute with a caller whose header tenant
// (and RLS GUC, as GUCBridgeMiddleware sets it) is headerTenant.
func servePT(t *testing.T, cfg PathTenantConfig, pathID string, rc *requestctx.RequestContext) (*httptest.ResponseRecorder, *seen) {
	t.Helper()
	got := &seen{}
	r := gin.New()
	r.GET(ptRoute,
		func(c *gin.Context) {
			ctx := c.Request.Context()
			if rc != nil {
				ctx = requestctx.WithContext(ctx, rc)
				g, _ := pgcommon.GUCSetFromContext(ctx)
				g.TenantID = rc.TenantID.String()
				g.UserID = "iam-system"
				ctx = pgcommon.WithGUCSet(ctx, g)
			}
			c.Request = c.Request.WithContext(ctx)
			c.Next()
		},
		BindPathTenant(cfg),
		func(c *gin.Context) {
			got.called = true
			if g, ok := pgcommon.GUCSetFromContext(c.Request.Context()); ok {
				got.gucTID, got.gucUID = g.TenantID, g.UserID
			}
			if v, ok := requestctx.FromContext(c.Request.Context()); ok {
				got.rc, got.hasRC = *v, true
			}
			c.Status(http.StatusOK)
		})
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		strings.Replace(ptRoute, ":id", pathID, 1), nil)
	r.ServeHTTP(w, req)
	return w, got
}

func callerRC(headerTenant uuid.UUID) *requestctx.RequestContext {
	return &requestctx.RequestContext{
		UserID: uuid.New(), TenantID: headerTenant, Roles: []string{"commercial-system"},
		ClientIP: "10.0.0.7", UserAgent: "billing/1.2",
	}
}

func mismatchCount(action string) float64 {
	return testutil.ToFloat64(metrics.TenantHeaderPathMismatch.WithLabelValues(ptRoute, http.MethodGet, action))
}

// ── tenant ID validation ─────────────────────────────────────────────────

func TestParsePathTenant(t *testing.T) {
	id := uuid.New()
	got, err := parsePathTenant(id.String())
	require.NoError(t, err)
	assert.Equal(t, id, got)

	for _, bad := range []string{"", "not-a-uuid", "1234", id.String() + "x"} {
		_, err := parsePathTenant(bad)
		assert.Error(t, err, "%q must be rejected", bad)
	}
}

func TestBindPathTenant_InvalidTenantID_400NoHandler(t *testing.T) {
	w, got := servePT(t, PathTenantConfig{}, "not-a-uuid", callerRC(uuid.New()))
	assertErrorCode(t, w, http.StatusBadRequest, "VALIDATION_ERROR")
	assert.False(t, got.called, "handler must not run on an invalid path tenant")
}

// ── matching header ──────────────────────────────────────────────────────

func TestBindPathTenant_Match_BindsAndPreservesIdentity(t *testing.T) {
	ptInitMetrics(t)
	tenant := uuid.New()
	rc := callerRC(tenant)
	log := &ptLogger{}
	before := mismatchCount(metrics.TenantMismatchOverridden)

	w, got := servePT(t, PathTenantConfig{Logger: log}, tenant.String(), rc)
	require.Equal(t, http.StatusOK, w.Code)
	require.True(t, got.called)
	assert.Equal(t, tenant.String(), got.gucTID)
	assert.Equal(t, "iam-system", got.gucUID, "GUC user must be left as the caller's")
	assert.Equal(t, rc.UserID, got.rc.UserID)
	assert.Equal(t, rc.Roles, got.rc.Roles)
	assert.Empty(t, log.warns, "no warning when header and path agree")
	assert.Equal(t, before, mismatchCount(metrics.TenantMismatchOverridden))
}

// ── mismatch, strict off: path wins ──────────────────────────────────────

func TestBindPathTenant_Mismatch_PathOverridesHeader(t *testing.T) {
	ptInitMetrics(t)
	pathTenant, headerTenant := uuid.New(), uuid.New()
	rc := callerRC(headerTenant)

	w, got := servePT(t, PathTenantConfig{}, pathTenant.String(), rc)
	require.Equal(t, http.StatusOK, w.Code, "non-strict mode serves the request")
	require.True(t, got.called)
	assert.Equal(t, pathTenant.String(), got.gucTID, "RLS tenant must be the path tenant")
	require.True(t, got.hasRC)
	assert.Equal(t, pathTenant, got.rc.TenantID, "RequestContext tenant must be the path tenant")
	// Caller identity preserved.
	assert.Equal(t, rc.UserID, got.rc.UserID)
	assert.Equal(t, rc.Roles, got.rc.Roles)
	assert.Equal(t, rc.ClientIP, got.rc.ClientIP)
	assert.Equal(t, rc.UserAgent, got.rc.UserAgent)
	assert.Equal(t, "iam-system", got.gucUID)
	assert.Equal(t, headerTenant, rc.TenantID, "the caller's RequestContext must not be mutated in place")
}

func TestBindPathTenant_Mismatch_LogsWarningAndCounts(t *testing.T) {
	ptInitMetrics(t)
	pathTenant, headerTenant := uuid.New(), uuid.New()
	rc := callerRC(headerTenant)
	log := &ptLogger{}
	before := mismatchCount(metrics.TenantMismatchOverridden)

	_, _ = servePT(t, PathTenantConfig{Logger: log}, pathTenant.String(), rc)

	assert.Equal(t, before+1, mismatchCount(metrics.TenantMismatchOverridden))
	require.Len(t, log.warns, 1)
	assert.Equal(t, "tenant_header_path_mismatch", log.msgs[0])
	f := log.warns[0]
	assert.Equal(t, ptRoute, f["route"])
	assert.Equal(t, http.MethodGet, f["method"])
	assert.Contains(t, f, "request_id")
	assert.Equal(t, rc.UserID.String(), f["caller_user_id"])
	assert.Equal(t, rc.Roles, f["caller_roles"])
	assert.Equal(t, pathTenant.String(), f["path_tenant_id"])
	assert.Equal(t, headerTenant.String(), f["header_tenant_id"])
	assert.Equal(t, metrics.TenantMismatchOverridden, f["action"])
}

// ── strict mode ──────────────────────────────────────────────────────────

func TestBindPathTenant_Strict_Mismatch_400ProblemNoHandler(t *testing.T) {
	ptInitMetrics(t)
	log := &ptLogger{}
	before := mismatchCount(metrics.TenantMismatchRejected)

	w, got := servePT(t, PathTenantConfig{Strict: true, Logger: log}, uuid.New().String(), callerRC(uuid.New()))

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.False(t, got.called, "strict mode must not execute the handler")
	assert.Contains(t, w.Header().Get("Content-Type"), "application/problem+json")
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "TENANT_MISMATCH", body["errorCode"])
	assert.True(t, strings.HasSuffix(body["type"].(string), "/iam/tenant-mismatch"), "type: %v", body["type"])
	assert.EqualValues(t, http.StatusBadRequest, body["status"])
	assert.Equal(t, before+1, mismatchCount(metrics.TenantMismatchRejected))
	require.Len(t, log.warns, 1)
	assert.Equal(t, metrics.TenantMismatchRejected, log.warns[0]["action"])
}

func TestBindPathTenant_Strict_Match_Passes(t *testing.T) {
	tenant := uuid.New()
	w, got := servePT(t, PathTenantConfig{Strict: true}, tenant.String(), callerRC(tenant))
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, got.called)
	assert.Equal(t, tenant.String(), got.gucTID)
}

// No RequestContext (RequireInternalCaller would already have refused, but
// the middleware must not panic): binds the path tenant and continues.
func TestBindPathTenant_NoRequestContext_BindsAndContinues(t *testing.T) {
	tenant := uuid.New()
	w, got := servePT(t, PathTenantConfig{Strict: true}, tenant.String(), nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, tenant.String(), got.gucTID)
	assert.False(t, got.hasRC)
}

// ── I-8: outside the group, binds ?tenant_id= itself ─────────────────────

type ptAuthZRepo struct{ gucTID string }

func (r *ptAuthZRepo) FindMembershipProjection(ctx context.Context, _, _ uuid.UUID) (*port.MembershipProjectionRow, error) {
	if g, ok := pgcommon.GUCSetFromContext(ctx); ok {
		r.gucTID = g.TenantID
	}
	return nil, nil
}

// I-8's :id is the user, so it is not behind BindPathTenant; the handler
// binds the ?tenant_id= tenant as the RLS tenant over a mismatched header.
func TestGetMemberships_BindsQueryTenantAsRLSTenant(t *testing.T) {
	queryTenant := uuid.New()
	repo := &ptAuthZRepo{}
	h := &InternalHandler{authz: service.NewAuthZService(repo, nil, nil, nil)}

	c, _ := buildCtx(http.MethodGet, "/?tenant_id="+queryTenant.String(), ``, systemCtx())
	g, _ := pgcommon.GUCSetFromContext(c.Request.Context())
	g.TenantID = uuid.New().String() // header tenant differs
	c.Request = c.Request.WithContext(pgcommon.WithGUCSet(c.Request.Context(), g))
	setParams(c, "id", uuid.New().String())

	h.GetMemberships(c)
	assert.Equal(t, queryTenant.String(), repo.gucTID, "?tenant_id= must be the RLS tenant for I-8")
}
