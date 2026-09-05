package catalogadmin

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// envOr / envDurationMs are the composition-root helpers used by New().
// Pure functions; test isolation via t.Setenv (auto-reverts).

func TestEnvOr_ReturnsValueWhenSet(t *testing.T) {
	t.Setenv("CATALOG_TEST_KEY_1", "custom")
	assert.Equal(t, "custom", envOr("CATALOG_TEST_KEY_1", "default"))
}

func TestEnvOr_ReturnsDefaultWhenUnset(t *testing.T) {
	_ = os.Unsetenv("CATALOG_TEST_KEY_UNSET")
	assert.Equal(t, "default", envOr("CATALOG_TEST_KEY_UNSET", "default"))
}

func TestEnvOr_EmptyStringTreatedAsUnset(t *testing.T) {
	t.Setenv("CATALOG_TEST_KEY_2", "")
	assert.Equal(t, "fallback", envOr("CATALOG_TEST_KEY_2", "fallback"))
}

func TestEnvDurationMs_ParsesMillisecondsInteger(t *testing.T) {
	t.Setenv("CATALOG_ADMIN_TIMEOUT_MS_TEST", "2500")
	assert.Equal(t, 2500*time.Millisecond, envDurationMs("CATALOG_ADMIN_TIMEOUT_MS_TEST", time.Second))
}

func TestEnvDurationMs_ParsesGoDurationString(t *testing.T) {
	t.Setenv("CATALOG_ADMIN_TIMEOUT_MS_TEST", "1.5s")
	assert.Equal(t, 1500*time.Millisecond, envDurationMs("CATALOG_ADMIN_TIMEOUT_MS_TEST", time.Second))
}

func TestEnvDurationMs_UnsetReturnsDefault(t *testing.T) {
	_ = os.Unsetenv("CATALOG_ADMIN_TIMEOUT_MS_TEST")
	assert.Equal(t, 3*time.Second, envDurationMs("CATALOG_ADMIN_TIMEOUT_MS_TEST", 3*time.Second))
}

func TestEnvDurationMs_InvalidValueReturnsDefault(t *testing.T) {
	t.Setenv("CATALOG_ADMIN_TIMEOUT_MS_TEST", "not-a-duration")
	assert.Equal(t, 5*time.Second, envDurationMs("CATALOG_ADMIN_TIMEOUT_MS_TEST", 5*time.Second))
}

func TestEnvDurationMs_NegativeIntFallsBackToDefault(t *testing.T) {
	t.Setenv("CATALOG_ADMIN_TIMEOUT_MS_TEST", "-100")
	assert.Equal(t, time.Second, envDurationMs("CATALOG_ADMIN_TIMEOUT_MS_TEST", time.Second))
}

func TestNewHTTPClient_NilLoggerAndZeroTimeoutDefault(t *testing.T) {
	c := NewHTTPClient("http://x", 0, nil)
	assert.NotNil(t, c)
	assert.NotNil(t, c.logger, "nil logger should be replaced by slog.Default()")
	assert.Equal(t, "http://x", c.baseURL)
	assert.NotZero(t, c.client.Timeout)
}

func TestNew_ReadsEnvAndConstructs(t *testing.T) {
	t.Setenv("CATALOG_ADMIN_BASE_URL", "http://catalog.local")
	t.Setenv("CATALOG_ADMIN_TIMEOUT_MS", "500")
	client := New(nil)
	assert.NotNil(t, client)
}

func TestNew_NoEnvBuildsUnconfiguredClient(t *testing.T) {
	_ = os.Unsetenv("CATALOG_ADMIN_BASE_URL")
	_ = os.Unsetenv("CATALOG_ADMIN_TIMEOUT_MS")
	client := New(nil)
	assert.NotNil(t, client)
}
