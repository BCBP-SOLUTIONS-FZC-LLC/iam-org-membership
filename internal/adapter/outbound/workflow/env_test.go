package workflow

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// envOr / envDurationMs are the composition-root helpers used by New().
// Pure functions; test isolation via t.Setenv (auto-reverts).

func TestEnvOr_ReturnsValueWhenSet(t *testing.T) {
	t.Setenv("WORKFLOW_TEST_KEY_1", "custom")
	assert.Equal(t, "custom", envOr("WORKFLOW_TEST_KEY_1", "default"))
}

func TestEnvOr_ReturnsDefaultWhenUnset(t *testing.T) {
	_ = os.Unsetenv("WORKFLOW_TEST_KEY_UNSET")
	assert.Equal(t, "default", envOr("WORKFLOW_TEST_KEY_UNSET", "default"))
}

func TestEnvOr_EmptyStringTreatedAsUnset(t *testing.T) {
	t.Setenv("WORKFLOW_TEST_KEY_2", "")
	assert.Equal(t, "fallback", envOr("WORKFLOW_TEST_KEY_2", "fallback"))
}

func TestEnvDurationMs_ParsesMillisecondsInteger(t *testing.T) {
	t.Setenv("WORKFLOW_TIMEOUT_MS", "2500")
	assert.Equal(t, 2500*time.Millisecond, envDurationMs("WORKFLOW_TIMEOUT_MS", time.Second))
}

func TestEnvDurationMs_ParsesGoDurationString(t *testing.T) {
	// Fallback path: not an int, but is a valid Go duration.
	t.Setenv("WORKFLOW_TIMEOUT_MS", "1.5s")
	assert.Equal(t, 1500*time.Millisecond, envDurationMs("WORKFLOW_TIMEOUT_MS", time.Second))
}

func TestEnvDurationMs_UnsetReturnsDefault(t *testing.T) {
	_ = os.Unsetenv("WORKFLOW_TIMEOUT_MS")
	assert.Equal(t, 3*time.Second, envDurationMs("WORKFLOW_TIMEOUT_MS", 3*time.Second))
}

func TestEnvDurationMs_InvalidValueReturnsDefault(t *testing.T) {
	t.Setenv("WORKFLOW_TIMEOUT_MS", "not-a-duration")
	assert.Equal(t, 5*time.Second, envDurationMs("WORKFLOW_TIMEOUT_MS", 5*time.Second))
}

func TestEnvDurationMs_NegativeIntFallsBackToDefault(t *testing.T) {
	// Guard: n>0 gate — a negative-as-int must not be honored.
	t.Setenv("WORKFLOW_TIMEOUT_MS", "-100")
	assert.Equal(t, time.Second, envDurationMs("WORKFLOW_TIMEOUT_MS", time.Second))
}

// Different key exercises the parameter — guards against future refactors
// that assume a single hardcoded key and lets linters see key as varying.
func TestEnvDurationMs_AlternateKeyName(t *testing.T) {
	t.Setenv("SOME_OTHER_TIMEOUT_MS", "750")
	assert.Equal(t, 750*time.Millisecond,
		envDurationMs("SOME_OTHER_TIMEOUT_MS", time.Second))
}

// New() reads env directly and delegates to NewHTTPClient. Cover it by
// setting env vars and asserting the resulting client is non-nil.
func TestNew_ReadsEnvAndConstructs(t *testing.T) {
	t.Setenv("WORKFLOW_SERVICE_BASE_URL", "http://wf.local")
	t.Setenv("WORKFLOW_TIMEOUT_MS", "500")
	client := New()
	assert.NotNil(t, client)
}

func TestNew_NoEnvBuildsUnconfiguredClient(t *testing.T) {
	_ = os.Unsetenv("WORKFLOW_SERVICE_BASE_URL")
	_ = os.Unsetenv("WORKFLOW_TIMEOUT_MS")
	// The unconfigured (baseURL="") client is the WFI-13 fail-open shape.
	client := New()
	assert.NotNil(t, client)
}
