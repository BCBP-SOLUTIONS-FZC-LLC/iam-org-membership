// env_helpers_test.go covers envOr and envDurationMs (package-private helpers)
// and the New() constructor path for tokenservice/http_client.go.
package tokenservice

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ── envOr ─────────────────────────────────────────────────────────────────

func TestEnvOr_SetEnv_ReturnsEnvValue(t *testing.T) {
	t.Setenv("TS_TEST_KEY_ENVOR", "from-env")
	got := envOr("TS_TEST_KEY_ENVOR", "default")
	assert.Equal(t, "from-env", got)
}

func TestEnvOr_UnsetEnv_ReturnsDefault(t *testing.T) {
	got := envOr("TS_TEST_KEY_ENVOR_UNSET_XYZ999", "my-default")
	assert.Equal(t, "my-default", got)
}

// ── envDurationMs ─────────────────────────────────────────────────────────

func TestEnvDurationMs_UnsetEnv_ReturnsDefault(t *testing.T) {
	def := 5 * time.Second
	got := envDurationMs("TS_TEST_DURATION_UNSET_XYZ999", def)
	assert.Equal(t, def, got)
}

func TestEnvDurationMs_ValidInteger_ReturnsParsedMs(t *testing.T) {
	t.Setenv("TS_TEST_DURATION_MS", "2000")
	got := envDurationMs("TS_TEST_DURATION_MS", time.Second)
	assert.Equal(t, 2000*time.Millisecond, got)
}

func TestEnvDurationMs_ValidDuration_ReturnsParsedDuration(t *testing.T) {
	t.Setenv("TS_TEST_DURATION_STR", "3s")
	got := envDurationMs("TS_TEST_DURATION_STR", time.Second)
	assert.Equal(t, 3*time.Second, got)
}

func TestEnvDurationMs_InvalidValue_ReturnsDefault(t *testing.T) {
	t.Setenv("TS_TEST_DURATION_BAD", "not-a-duration")
	def := 7 * time.Second
	got := envDurationMs("TS_TEST_DURATION_BAD", def)
	assert.Equal(t, def, got)
}

func TestNew_NoBaseURLEnv_LeavesBaseURLEmpty(t *testing.T) {
	// Ensure a clean env for this test regardless of ambient state.
	t.Setenv("TOKEN_SERVICE_BASE_URL", "")
	c := New(nil)
	assert.Equal(t, "", c.baseURL)
}
