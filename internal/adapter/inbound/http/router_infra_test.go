// router_infra_test.go — whitebox unit tests for the infra handlers in
// router.go: healthz and readyz. These run in the http package itself to
// access the healthHandlers type and its methods directly.
//
// The tests construct a minimal gin.Engine with the handlers registered,
// make an in-process HTTP request, and verify the response status and body.
// No Docker / external dependencies required.
package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubPinger implements the Pinger interface so tests can inject health
// pass/fail scenarios without importing any concrete infrastructure type.
type stubPinger struct{ err error }

func (p *stubPinger) Health(_ context.Context) error { return p.err }

// pingOK returns a Pinger that always succeeds.
func pingOK() *stubPinger { return &stubPinger{err: nil} }

// pingFail returns a Pinger that always returns the supplied error.
func pingFail(err error) *stubPinger { return &stubPinger{err: err} }

// setupHealthEngine wires the infra routes onto a gin.Engine in TestMode
// and returns the engine and the registered handlers.
func setupHealthEngine(h *healthHandlers) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/healthz", h.healthz)
	r.GET("/readyz", h.readyz)
	return r
}

// ── /healthz ─────────────────────────────────────────────────────────────

// TestHealthz_AlwaysReturns200 verifies that /healthz is a pure liveness
// check: it never inspects dependencies and always returns 200 ok.
func TestHealthz_AlwaysReturns200(t *testing.T) {
	h := &healthHandlers{
		postgres: pingFail(errors.New("db down")),
		cache:    pingFail(errors.New("cache down")),
		outbox:   pingFail(errors.New("outbox down")),
	}
	eng := setupHealthEngine(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil)
	eng.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"status":"ok"`)
}

// ── /readyz — happy path ──────────────────────────────────────────────────

// TestReadyz_AllHealthy_Returns200 verifies that when all pingers succeed
// /readyz returns 200 with status="ready".
func TestReadyz_AllHealthy_Returns200(t *testing.T) {
	h := &healthHandlers{
		postgres: pingOK(),
		cache:    pingOK(),
		outbox:   pingOK(),
	}
	eng := setupHealthEngine(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	eng.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"ready"`)
	assert.Contains(t, body, `"database":"ok"`)
	assert.Contains(t, body, `"cache":"ok"`)
	assert.Contains(t, body, `"outbox":"ok"`)
}

// ── /readyz — postgres down ───────────────────────────────────────────────

// TestReadyz_PostgresDown_Returns503 verifies that when Postgres is unhealthy
// /readyz returns 503 with status="not ready" and database="down".
func TestReadyz_PostgresDown_Returns503(t *testing.T) {
	h := &healthHandlers{
		postgres: pingFail(errors.New("connection refused")),
		cache:    pingOK(),
		outbox:   pingOK(),
	}
	eng := setupHealthEngine(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	eng.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"not ready"`)
	assert.Contains(t, body, `"database":"down"`)
}

// ── /readyz — cache down ──────────────────────────────────────────────────

// TestReadyz_CacheDown_Returns503 verifies that when Valkey is unhealthy
// /readyz returns 503 (cache is advisory everywhere else, but /readyz fails
// so the pod is removed from rotation — CACHE-9 comment in router.go).
func TestReadyz_CacheDown_Returns503(t *testing.T) {
	h := &healthHandlers{
		postgres: pingOK(),
		cache:    pingFail(errors.New("valkey timeout")),
		outbox:   pingOK(),
	}
	eng := setupHealthEngine(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	eng.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), `"cache":"down"`)
}

// ── /readyz — outbox down ─────────────────────────────────────────────────

// TestReadyz_OutboxDown_Returns503 verifies that when the outbox runner
// reports unhealthy, /readyz returns 503 with outbox="initialising".
func TestReadyz_OutboxDown_Returns503(t *testing.T) {
	h := &healthHandlers{
		postgres: pingOK(),
		cache:    pingOK(),
		outbox:   pingFail(errors.New("outbox not ready")),
	}
	eng := setupHealthEngine(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	eng.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), `"outbox":"initialising"`)
}

// ── /readyz — sysPostgres optional check ─────────────────────────────────

// TestReadyz_SysPostgresNil_Skipped verifies that when sysPostgres is nil
// (single-role local dev), the sys_database check is omitted from the
// response and the overall result is not affected.
func TestReadyz_SysPostgresNil_Skipped(t *testing.T) {
	h := &healthHandlers{
		postgres:    pingOK(),
		sysPostgres: nil, // optional — see RouterConfig.SysPostgres
		cache:       pingOK(),
		outbox:      pingOK(),
	}
	eng := setupHealthEngine(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	eng.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "sys_database",
		"sys_database key must not appear when sysPostgres is nil")
}

// TestReadyz_SysPostgresDown_Returns503 verifies that when sysPostgres is set
// and unhealthy, /readyz returns 503 with sys_database="down".
func TestReadyz_SysPostgresDown_Returns503(t *testing.T) {
	h := &healthHandlers{
		postgres:    pingOK(),
		sysPostgres: pingFail(errors.New("bypassrls pool unhealthy")),
		cache:       pingOK(),
		outbox:      pingOK(),
	}
	eng := setupHealthEngine(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	eng.ServeHTTP(w, req)

	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), `"sys_database":"down"`)
}

// TestReadyz_SysPostgresOK_IncludedInChecks verifies that when sysPostgres
// is set and healthy, sys_database="ok" appears in the checks.
func TestReadyz_SysPostgresOK_IncludedInChecks(t *testing.T) {
	h := &healthHandlers{
		postgres:    pingOK(),
		sysPostgres: pingOK(),
		cache:       pingOK(),
		outbox:      pingOK(),
	}
	eng := setupHealthEngine(h)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	eng.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"sys_database":"ok"`)
}

// ── registerInfraRoutes ────────────────────────────────────────────────────

// TestRegisterInfraRoutes_HealthzAndReadyzRegistered verifies that calling
// registerInfraRoutes wires /healthz and /readyz onto the engine.
func TestRegisterInfraRoutes_HealthzAndReadyzRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	cfg := RouterConfig{
		Postgres: pingOK(),
		Cache:    pingOK(),
		Outbox:   pingOK(),
	}
	registerInfraRoutes(r, cfg)

	// /healthz
	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code, "registerInfraRoutes must register /healthz")

	// /readyz
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code, "registerInfraRoutes must register /readyz")
}

// ── registerDocsRoutes ─────────────────────────────────────────────────────

// TestRegisterDocsRoutes_InactiveDocs_NoRoutesAdded verifies that when
// DocsConfig.active() returns false (production + Enabled=false), the docs
// routes are not registered and /swagger/index.html returns 404.
func TestRegisterDocsRoutes_InactiveDocs_NoRoutesAdded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	cfg := RouterConfig{
		Docs: DocsConfig{Environment: "production", Enabled: false},
	}
	registerDocsRoutes(r, cfg)

	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code,
		"inactive docs must not register any route")
}

// TestRegisterDocsRoutes_NonProduction_AsyncAPIReachable verifies that in
// non-production the /asyncapi route is registered and reachable (the handler
// itself may return 200 or require further context, but the route must exist).
func TestRegisterDocsRoutes_NonProduction_AsyncAPIReachable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	cfg := RouterConfig{
		Docs:      DocsConfig{Environment: "staging", Enabled: false},
		GinConfig: gincommon.Config{},
	}
	registerDocsRoutes(r, cfg)

	// /asyncapi route must exist (not 404).
	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", nil)
	r.ServeHTTP(w, req)
	assert.NotEqual(t, http.StatusNotFound, w.Code,
		"staging docs must register /asyncapi")
}

// TestRegisterDocsRoutes_Production_WithAuthToken_BlocksUnauth verifies that
// in production mode with an auth token set, requests without the correct
// Authorization header are rejected with 401.
func TestRegisterDocsRoutes_Production_WithAuthToken_BlocksUnauth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	cfg := RouterConfig{
		Docs: DocsConfig{
			Environment: "production",
			Enabled:     true,
			AuthToken:   "secret-token",
		},
	}
	registerDocsRoutes(r, cfg)

	// No Authorization header → must be blocked with 401.
	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", nil)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"production docs without auth token must return 401")
}

// TestRegisterDocsRoutes_Production_WithAuthToken_AllowsValidToken verifies
// that a correct Bearer token passes the auth middleware in production.
func TestRegisterDocsRoutes_Production_WithAuthToken_AllowsValidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	cfg := RouterConfig{
		Docs: DocsConfig{
			Environment: "production",
			Enabled:     true,
			AuthToken:   "secret-token",
		},
	}
	registerDocsRoutes(r, cfg)

	// Correct Authorization header → route passes auth and reaches the handler.
	w := httptest.NewRecorder()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	r.ServeHTTP(w, req)
	// The AsyncAPI handler runs — expect any non-401 response.
	assert.NotEqual(t, http.StatusUnauthorized, w.Code,
		"valid bearer token must pass the production auth middleware")
}

// TestRegisterDocsRoutes_Production_NoAuthToken_NoLogger_NoPanic verifies
// that when in production with Enabled=true but no auth token and no logger,
// the middleware is a no-op (no panic).
func TestRegisterDocsRoutes_Production_NoAuthToken_NoLogger_NoPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	cfg := RouterConfig{
		Docs: DocsConfig{
			Environment: "production",
			Enabled:     true,
			AuthToken:   "", // no auth token → fallback to no-op middleware
		},
		GinConfig: gincommon.Config{Logger: nil}, // no logger → warn is silently skipped
	}
	assert.NotPanics(t, func() {
		registerDocsRoutes(r, cfg)
	})
}

// TestDocsConfig_ActiveOutsideProduction verifies that DocsConfig.active()
// returns true in non-production environments regardless of Enabled.
func TestDocsConfig_ActiveOutsideProduction(t *testing.T) {
	d := DocsConfig{Environment: "staging", Enabled: false}
	assert.True(t, d.active(), "docs must be active outside production")
}

// TestDocsConfig_InProductionEnabledTrue verifies that production + Enabled=true
// is active.
func TestDocsConfig_InProductionEnabledTrue(t *testing.T) {
	d := DocsConfig{Environment: "production", Enabled: true}
	assert.True(t, d.active())
}

// TestDocsConfig_InProductionEnabledFalse verifies that production + Enabled=false
// is not active.
func TestDocsConfig_InProductionEnabledFalse(t *testing.T) {
	d := DocsConfig{Environment: "production", Enabled: false}
	assert.False(t, d.active())
}
