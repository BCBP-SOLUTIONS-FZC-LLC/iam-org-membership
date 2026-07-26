package userprofile

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGetenv_ReturnsValueWhenSet(t *testing.T) {
	t.Setenv("UP_TEST_KEY_1", "custom")
	assert.Equal(t, "custom", getenv("UP_TEST_KEY_1", "default"))
}

func TestGetenv_ReturnsDefaultWhenUnset(t *testing.T) {
	_ = os.Unsetenv("UP_TEST_KEY_UNSET")
	assert.Equal(t, "default", getenv("UP_TEST_KEY_UNSET", "default"))
}

func TestGetenv_EmptyStringTreatedAsUnset(t *testing.T) {
	t.Setenv("UP_TEST_KEY_2", "")
	assert.Equal(t, "fallback", getenv("UP_TEST_KEY_2", "fallback"))
}

func TestGetenvDuration_ParsesMillisecondsInteger(t *testing.T) {
	t.Setenv("UP_TIMEOUT_MS", "2500")
	assert.Equal(t, 2500*time.Millisecond, getenvDuration("UP_TIMEOUT_MS", time.Second))
}

func TestGetenvDuration_ParsesGoDurationString(t *testing.T) {
	t.Setenv("UP_TIMEOUT_MS", "1.5s")
	assert.Equal(t, 1500*time.Millisecond, getenvDuration("UP_TIMEOUT_MS", time.Second))
}

func TestGetenvDuration_UnsetReturnsDefault(t *testing.T) {
	_ = os.Unsetenv("UP_TIMEOUT_MS")
	assert.Equal(t, 3*time.Second, getenvDuration("UP_TIMEOUT_MS", 3*time.Second))
}

func TestGetenvDuration_InvalidReturnsDefault(t *testing.T) {
	t.Setenv("UP_TIMEOUT_MS", "not-a-duration")
	assert.Equal(t, 5*time.Second, getenvDuration("UP_TIMEOUT_MS", 5*time.Second))
}

func TestGetenvDuration_NegativeIntFallsBackToDefault(t *testing.T) {
	t.Setenv("UP_TIMEOUT_MS", "-100")
	assert.Equal(t, time.Second, getenvDuration("UP_TIMEOUT_MS", time.Second))
}

func TestNew_ReadsEnvAndConstructs(t *testing.T) {
	t.Setenv("USER_PROFILE_SERVICE_BASE_URL", "http://up.local")
	t.Setenv("USER_PROFILE_TIMEOUT_MS", "500")
	assert.NotNil(t, New())
}

func TestNew_UnconfiguredStillReturnsClient(t *testing.T) {
	_ = os.Unsetenv("USER_PROFILE_SERVICE_BASE_URL")
	_ = os.Unsetenv("USER_PROFILE_TIMEOUT_MS")
	assert.NotNil(t, New())
}
