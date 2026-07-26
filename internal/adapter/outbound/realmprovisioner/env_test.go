package realmprovisioner

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestEnvOr_ReturnsValueWhenSet(t *testing.T) {
	t.Setenv("RP_TEST_KEY_1", "custom")
	assert.Equal(t, "custom", envOr("RP_TEST_KEY_1", "default"))
}

func TestEnvOr_ReturnsDefaultWhenUnset(t *testing.T) {
	_ = os.Unsetenv("RP_TEST_KEY_UNSET")
	assert.Equal(t, "default", envOr("RP_TEST_KEY_UNSET", "default"))
}

func TestEnvOr_EmptyStringTreatedAsUnset(t *testing.T) {
	t.Setenv("RP_TEST_KEY_2", "")
	assert.Equal(t, "fallback", envOr("RP_TEST_KEY_2", "fallback"))
}

func TestEnvDurationMs_ParsesMillisecondsInteger(t *testing.T) {
	t.Setenv("RP_TIMEOUT_MS", "2500")
	assert.Equal(t, 2500*time.Millisecond, envDurationMs("RP_TIMEOUT_MS", time.Second))
}

func TestEnvDurationMs_ParsesGoDurationString(t *testing.T) {
	t.Setenv("RP_TIMEOUT_MS", "1.5s")
	assert.Equal(t, 1500*time.Millisecond, envDurationMs("RP_TIMEOUT_MS", time.Second))
}

func TestEnvDurationMs_UnsetReturnsDefault(t *testing.T) {
	_ = os.Unsetenv("RP_TIMEOUT_MS")
	assert.Equal(t, 3*time.Second, envDurationMs("RP_TIMEOUT_MS", 3*time.Second))
}

func TestEnvDurationMs_InvalidReturnsDefault(t *testing.T) {
	t.Setenv("RP_TIMEOUT_MS", "not-a-duration")
	assert.Equal(t, 5*time.Second, envDurationMs("RP_TIMEOUT_MS", 5*time.Second))
}

func TestEnvDurationMs_NegativeIntFallsBackToDefault(t *testing.T) {
	t.Setenv("RP_TIMEOUT_MS", "-100")
	assert.Equal(t, time.Second, envDurationMs("RP_TIMEOUT_MS", time.Second))
}

func TestNew_ReadsEnvAndConstructs(t *testing.T) {
	t.Setenv("REALM_PROVISIONER_BASE_URL", "http://rp.local")
	t.Setenv("REALM_PROVISIONER_TIMEOUT_MS", "500")
	assert.NotNil(t, New())
}

func TestNew_UnconfiguredStillReturnsClient(t *testing.T) {
	_ = os.Unsetenv("REALM_PROVISIONER_BASE_URL")
	_ = os.Unsetenv("REALM_PROVISIONER_TIMEOUT_MS")
	assert.NotNil(t, New())
}
