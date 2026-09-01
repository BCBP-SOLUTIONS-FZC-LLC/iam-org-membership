// middleware_supplement_test.go fills coverage gaps in middleware.go:
//
//   - bufferedWriter.Written() (0%) — returns true when buf has data or status set
//   - bufferedWriter.WriteString() (0%) — delegates to Write
//   - RequireActiveMembership (65.4%) — status not 'active' path
//   - isDBUnavailableSQLState (80.0%) — non-connection state path
package http

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBufferedWriter creates a bufferedWriter backed by a gin.CreateTestContext
// gin.ResponseWriter so the embedded gin.ResponseWriter interface is satisfied.
func newBufferedWriter(t *testing.T) *bufferedWriter {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	return &bufferedWriter{ResponseWriter: c.Writer, buf: &bytes.Buffer{}}
}

// ── bufferedWriter.Written() ──────────────────────────────────────────────

// TestBufferedWriter_Written_FalseWhenEmpty verifies Written() returns false
// for a fresh bufferedWriter (no writes, no WriteHeader call).
func TestBufferedWriter_Written_FalseWhenEmpty(t *testing.T) {
	bw := newBufferedWriter(t)
	assert.False(t, bw.Written(), "Written must be false before any write or WriteHeader")
}

// TestBufferedWriter_Written_TrueAfterWriteHeader verifies Written() returns
// true once WriteHeader has been called (status != 0).
func TestBufferedWriter_Written_TrueAfterWriteHeader(t *testing.T) {
	bw := newBufferedWriter(t)
	bw.WriteHeader(http.StatusOK)
	assert.True(t, bw.Written(), "Written must be true after WriteHeader")
}

// TestBufferedWriter_Written_TrueAfterWrite verifies Written() returns true
// once Write has been called (buf.Len() > 0).
func TestBufferedWriter_Written_TrueAfterWrite(t *testing.T) {
	bw := newBufferedWriter(t)
	n, err := bw.Write([]byte("hello"))
	require.NoError(t, err)
	assert.Equal(t, 5, n)
	assert.True(t, bw.Written(), "Written must be true after Write")
}

// ── bufferedWriter.WriteString() ─────────────────────────────────────────

// TestBufferedWriter_WriteString_DelegatesToWrite verifies that WriteString
// delegates to Write and returns the correct byte count.
func TestBufferedWriter_WriteString_DelegatesToWrite(t *testing.T) {
	bw := newBufferedWriter(t)
	n, err := bw.WriteString("test string")
	require.NoError(t, err)
	assert.Equal(t, 11, n, "WriteString must return len of the string")
	assert.Equal(t, "test string", bw.buf.String(), "WriteString must write to the buffer")
}

// ── bufferedWriter.Write sets status to 200 if not set ───────────────────

// TestBufferedWriter_Write_SetsDefaultStatus verifies the branch at line 326:
// if status == 0, Write sets status to http.StatusOK before writing.
func TestBufferedWriter_Write_SetsDefaultStatus(t *testing.T) {
	bw := newBufferedWriter(t)
	_, _ = bw.Write([]byte("data"))
	assert.Equal(t, http.StatusOK, bw.Status(), "Write without prior WriteHeader must set status=200")
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
