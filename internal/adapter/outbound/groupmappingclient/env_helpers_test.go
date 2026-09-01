// env_helpers_test.go fills coverage gaps in the envOr and envDurationMs
// helpers (package-private) for groupmappingclient/http_client.go.
package groupmappingclient

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ── envOr ─────────────────────────────────────────────────────────────────

func TestEnvOr_SetEnv_ReturnsEnvValue(t *testing.T) {
	t.Setenv("GM_TEST_KEY_ENVOR", "from-env")
	got := envOr("GM_TEST_KEY_ENVOR", "default")
	assert.Equal(t, "from-env", got)
}

func TestEnvOr_UnsetEnv_ReturnsDefault(t *testing.T) {
	got := envOr("GM_TEST_KEY_ENVOR_UNSET_XYZ123", "my-default")
	assert.Equal(t, "my-default", got)
}

// ── envDurationMs ─────────────────────────────────────────────────────────

func TestEnvDurationMs_UnsetEnv_ReturnsDefault(t *testing.T) {
	def := 5 * time.Second
	got := envDurationMs("GM_TEST_DURATION_UNSET_XYZ123", def)
	assert.Equal(t, def, got)
}

func TestEnvDurationMs_ValidInteger_ReturnsParsedMs(t *testing.T) {
	t.Setenv("GM_TEST_DURATION_MS", "2000")
	got := envDurationMs("GM_TEST_DURATION_MS", time.Second)
	assert.Equal(t, 2000*time.Millisecond, got)
}

func TestEnvDurationMs_ValidDuration_ReturnsParsedDuration(t *testing.T) {
	t.Setenv("GM_TEST_DURATION_STR", "3s")
	got := envDurationMs("GM_TEST_DURATION_STR", time.Second)
	assert.Equal(t, 3*time.Second, got)
}

func TestEnvDurationMs_InvalidValue_ReturnsDefault(t *testing.T) {
	t.Setenv("GM_TEST_DURATION_BAD", "not-a-duration")
	def := 7 * time.Second
	got := envDurationMs("GM_TEST_DURATION_BAD", def)
	assert.Equal(t, def, got)
}
