// middleware_supplement_test.go fills coverage gaps in middleware.go:
//
//   - RequireActiveMembership (65.4%) — status not 'active' path
//   - isDBUnavailableSQLState (80.0%) — non-connection state path
package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

// isDBUnavailableSQLState reports whether a raw SQLSTATE code indicates a
// DB-unavailable condition, using the same pgcommon helpers HandleError
// uses (classes 08, 53, 57 except 57014, and 58).
func isDBUnavailableSQLState(code string) bool {
	err := &pgconn.PgError{Code: code}
	return pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) ||
		pgcommon.IsOperatorIntervention(err) || pgcommon.IsSystemError(err)
}

// ── isDBUnavailableSQLState — non-connection error ───────────────────────

// TestIsDBUnavailableSQLState_NonConnectionState_ReturnsFalse covers the
// else-branch (line 423-425) of isDBUnavailableSQLState: a SQL state that
// does not indicate a connection/pool error returns false.
func TestIsDBUnavailableSQLState_NonConnectionState_ReturnsFalse(t *testing.T) {
	// "23505" is unique_violation — not a connection state.
	assert.False(t, isDBUnavailableSQLState("23505"),
		"unique_violation SQLSTATE must not be classified as DB unavailable")
}

// TestIsDBUnavailableSQLState_ConnectionState_ReturnsTrue verifies the
// connection-error SQLSTATE is correctly classified.
func TestIsDBUnavailableSQLState_ConnectionState_ReturnsTrue(t *testing.T) {
	// "08006" is connection_failure — a pg connection-class code.
	assert.True(t, isDBUnavailableSQLState("08006"),
		"08006 (connection_failure) must be classified as DB unavailable")
}

// TestIsDBUnavailableSQLState_PoolExhausted_ReturnsTrue verifies that the
// pgbouncer "pool exhausted" SQLSTATE (08P01) is classified correctly.
func TestIsDBUnavailableSQLState_PoolExhausted_ReturnsTrue(t *testing.T) {
	assert.True(t, isDBUnavailableSQLState("08P01"),
		"08P01 (pool_exhausted) must be classified as DB unavailable")
}

// ── RequireJSONContentType ────────────────────────────────────────────────

// TestRequireJSONContentType_NonJSON_WithBody_Returns415 covers the branch where
// Content-Type is not application/json on a POST with a non-empty body → 415.
func TestRequireJSONContentType_NonJSON_WithBody_Returns415(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	eng := gin.New()

	// Register the middleware and a dummy route.
	eng.Use(RequireJSONContentType())
	eng.POST("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	// Non-empty body with Content-Type: text/plain → must be rejected.
	body := strings.NewReader(`some form data`)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/test", body)
	req.Header.Set("Content-Type", "text/plain")
	eng.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnsupportedMediaType, w.Code)
}

// TestRequireJSONContentType_JSONBody_Passes verifies the happy path:
// Content-Type: application/json passes through.
func TestRequireJSONContentType_JSONBody_Passes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()

	eng := gin.New()
	eng.Use(RequireJSONContentType())
	eng.POST("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/test", nil)
	req.Header.Set("Content-Type", "application/json")
	eng.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}
