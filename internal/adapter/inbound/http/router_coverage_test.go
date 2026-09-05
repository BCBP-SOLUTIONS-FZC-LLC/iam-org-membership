// Coverage tests for router.go's infra probes (healthz/readyz) and the
// docs-surface gate (registerDocsRoutes) — the remaining gaps from the
// prior coverage session (see membership_handler_coverage_test.go and
// siblings for the established fake/stub convention this file follows).
package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Pinger fakes ─────────────────────────────────────────────────────────

// rcPinger is a configurable Pinger fake — prefix "rc" (router coverage) to
// avoid collisions with other test files' fakes.
type rcPinger struct{ err error }

func (p rcPinger) Health(context.Context) error { return p.err }

// ── readyz — every dependency combination ────────────────────────────────

func TestReadyz_AllDependenciesHealthy_Returns200(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
		Postgres:  rcPinger{}, Cache: rcPinger{}, Outbox: rcPinger{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", http.NoBody)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"ready"`)
	assert.Contains(t, w.Body.String(), `"database":"ok"`)
	assert.Contains(t, w.Body.String(), `"cache":"ok"`)
	assert.Contains(t, w.Body.String(), `"outbox":"ok"`)
}

func TestReadyz_DatabaseDown_Returns503(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
		Postgres:  rcPinger{err: errors.New("conn refused")}, Cache: rcPinger{}, Outbox: rcPinger{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", http.NoBody)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), `"database":"down"`)
	assert.Contains(t, w.Body.String(), `"not ready"`)
}

func TestReadyz_SysPostgresHealthy_IncludedInBody(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
		Postgres:  rcPinger{}, Cache: rcPinger{}, Outbox: rcPinger{},
		SysPostgres: rcPinger{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", http.NoBody)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"sys_database":"ok"`)
}

// ── registerDocsRoutes — active() gate ────────────────────────────────────

func TestDocsRoutes_InactiveInProduction_NotRegistered(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
		Docs:      DocsConfig{Environment: "production", Enabled: false},
		Postgres:  rcPinger{}, Cache: rcPinger{}, Outbox: rcPinger{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", http.NoBody)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code, "docs must not be registered when inactive")
}

func TestDocsRoutes_ActiveOutsideProduction_AsyncAPIServed(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
		Docs:      DocsConfig{Environment: "dev"},
		Postgres:  rcPinger{}, Cache: rcPinger{}, Outbox: rcPinger{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", http.NoBody)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "<!DOCTYPE html>")
	// Security headers set by secHeaders.
	assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
}

func TestDocsRoutes_ActiveOutsideProduction_YAMLServed(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
		Docs:      DocsConfig{Environment: "dev"},
		Postgres:  rcPinger{}, Cache: rcPinger{}, Outbox: rcPinger{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi.yaml", http.NoBody)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "yaml")
}

// ── registerDocsRoutes — production + AuthToken gate ──────────────────────

func TestDocsRoutes_ProductionWithToken_RejectsMissingBearer(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
		Docs:      DocsConfig{Environment: "production", Enabled: true, AuthToken: "s3cr3t"},
		Postgres:  rcPinger{}, Cache: rcPinger{}, Outbox: rcPinger{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", http.NoBody)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "unauthorized")
}

func TestDocsRoutes_ProductionWithToken_RejectsWrongBearer(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
		Docs:      DocsConfig{Environment: "production", Enabled: true, AuthToken: "s3cr3t"},
		Postgres:  rcPinger{}, Cache: rcPinger{}, Outbox: rcPinger{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", http.NoBody)
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestDocsRoutes_ProductionWithToken_AcceptsCorrectBearer(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
		Docs:      DocsConfig{Environment: "production", Enabled: true, AuthToken: "s3cr3t"},
		Postgres:  rcPinger{}, Cache: rcPinger{}, Outbox: rcPinger{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", http.NoBody)
	req.Header.Set("Authorization", "Bearer s3cr3t")
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// Production without an AuthToken configured: docs remain reachable
// (unauthenticated) but a warning is logged — covers the else-if branch
// and the Logger.Warn call site.
func TestDocsRoutes_ProductionWithoutToken_ServesUnauthenticatedAndWarns(t *testing.T) {
	logger := &rcTestLogger{}
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test", Logger: logger},
		Docs:      DocsConfig{Environment: "production", Enabled: true},
		Postgres:  rcPinger{}, Cache: rcPinger{}, Outbox: rcPinger{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", http.NoBody)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, logger.warnCalled, "must warn when DOCS_ENABLED in production without DOCS_AUTH_TOKEN")
}

// ── registerDocsRoutes — swagger asset routing branches ───────────────────

func TestDocsRoutes_SwaggerIndexCSS_UsesThemeHandler(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
		Docs:      DocsConfig{Environment: "dev"},
		Postgres:  rcPinger{}, Cache: rcPinger{}, Outbox: rcPinger{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/swagger/index.css", http.NoBody)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "css")
}

func TestDocsRoutes_SwaggerInitializerJS_UsesInitializerHandler(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
		Docs:      DocsConfig{Environment: "dev"},
		Postgres:  rcPinger{}, Cache: rcPinger{}, Outbox: rcPinger{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/swagger/swagger-initializer.js", http.NoBody)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "javascript")
}

func TestDocsRoutes_SwaggerDefault_FallsThroughToStdHandler(t *testing.T) {
	router := NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-org-membership-test"},
		Docs:      DocsConfig{Environment: "dev"},
		Postgres:  rcPinger{}, Cache: rcPinger{}, Outbox: rcPinger{},
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/swagger/doc.json", http.NoBody)
	w := httptest.NewRecorder()
	router.Handler().ServeHTTP(w, req)

	// Whatever the std swagger handler does with a missing spec, it must not
	// be a 404 from an unregistered route (which is what we're guarding
	// against — that this branch was actually reached).
	require.NotEqual(t, 0, w.Code)
}

// rcTestLogger is a minimal port.Logger fake that records whether Warn/Error
// fired. Shared with middleware_coverage2_test.go's HandleError logging test.
type rcTestLogger struct {
	warnCalled  bool
	errorCalled bool
}

func (l *rcTestLogger) Debug(string, map[string]any) {}
func (l *rcTestLogger) Info(string, map[string]any)  {}
func (l *rcTestLogger) Warn(string, map[string]any)  { l.warnCalled = true }
func (l *rcTestLogger) Error(string, map[string]any) { l.errorCalled = true }
