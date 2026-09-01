// env_helpers_test.go fills coverage gaps in the envOr and envDurationMs
// helpers (package-private), and the NewHTTPClient constructor for
// catalogadmin/http_client.go.
package catalogadmin

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ── envOr ─────────────────────────────────────────────────────────────────

// TestEnvOr_SetEnv_ReturnsEnvValue covers the branch: env var is set
// and non-empty → return the env value (not the default).
func TestEnvOr_SetEnv_ReturnsEnvValue(t *testing.T) {
	t.Setenv("CA_TEST_KEY_ENVOR", "from-env")
	got := envOr("CA_TEST_KEY_ENVOR", "default")
	assert.Equal(t, "from-env", got)
}

// TestEnvOr_UnsetEnv_ReturnsDefault covers the branch: env var is not set
// → return the default.
func TestEnvOr_UnsetEnv_ReturnsDefault(t *testing.T) {
	// Key is intentionally absent from env.
	got := envOr("CA_TEST_KEY_ENVOR_UNSET_XYZ123", "my-default")
	assert.Equal(t, "my-default", got)
}

// ── envDurationMs ─────────────────────────────────────────────────────────

// TestEnvDurationMs_UnsetEnv_ReturnsDefault covers: env var absent → default.
func TestEnvDurationMs_UnsetEnv_ReturnsDefault(t *testing.T) {
	def := 5 * time.Second
	got := envDurationMs("CA_TEST_DURATION_UNSET_XYZ123", def)
	assert.Equal(t, def, got)
}

// TestEnvDurationMs_ValidInteger_ReturnsParsedMs covers: env var is a valid
// positive integer → interpret as milliseconds.
func TestEnvDurationMs_ValidInteger_ReturnsParsedMs(t *testing.T) {
	t.Setenv("CA_TEST_DURATION_MS", "2000")
	got := envDurationMs("CA_TEST_DURATION_MS", time.Second)
	assert.Equal(t, 2000*time.Millisecond, got)
}

// TestEnvDurationMs_ValidDuration_ReturnsParsedDuration covers: env var is a
// valid Go duration string → parsed and returned.
func TestEnvDurationMs_ValidDuration_ReturnsParsedDuration(t *testing.T) {
	t.Setenv("CA_TEST_DURATION_STR", "3s")
	got := envDurationMs("CA_TEST_DURATION_STR", time.Second)
	assert.Equal(t, 3*time.Second, got)
}

// TestEnvDurationMs_InvalidValue_ReturnsDefault covers: env var is set but not
// parseable as an integer or duration → fall through to default.
func TestEnvDurationMs_InvalidValue_ReturnsDefault(t *testing.T) {
	t.Setenv("CA_TEST_DURATION_BAD", "not-a-duration")
	def := 7 * time.Second
	got := envDurationMs("CA_TEST_DURATION_BAD", def)
	assert.Equal(t, def, got)
}

// ── NewHTTPClient via New(nil) default path ───────────────────────────────

// TestNewHTTPClient_WithExplicitURL_ReturnsFunctionalClient verifies
// NewHTTPClient returns a non-nil *HTTPClient when given a non-empty URL.
func TestNewHTTPClient_WithExplicitURL_ReturnsFunctionalClient(t *testing.T) {
	c := NewHTTPClient("http://catalog.internal", 5*time.Second, slog.Default())
	assert.NotNil(t, c)
}
