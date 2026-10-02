package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveAppEnv_PrecedenceAndNormalisation(t *testing.T) {
	t.Setenv("APP_ENV", "")
	t.Setenv("ENVIRONMENT", "")
	assert.Equal(t, "dev", resolveAppEnv(), "nothing set → dev")

	t.Setenv("ENVIRONMENT", " Staging ")
	assert.Equal(t, "staging", resolveAppEnv(), "ENVIRONMENT is the fallback, trimmed and lowercased like gincommon")

	t.Setenv("APP_ENV", "PROD")
	assert.Equal(t, "prod", resolveAppEnv(), "APP_ENV wins")
}

func TestLoggerEnv_DevLikeEnvironmentsGetDevLogs(t *testing.T) {
	for _, env := range []string{"local", "dev", "test"} {
		assert.Equal(t, "dev", loggerEnv(env), env)
	}
	for _, env := range []string{"staging", "prod"} {
		assert.Equal(t, env, loggerEnv(env), env)
	}
}

func TestRunMigrationsAtStartup(t *testing.T) {
	for value, want := range map[string]bool{"": true, "true": true, "1": true, "false": false, "0": false, " FALSE ": false, "garbage": true} {
		t.Setenv("RUN_MIGRATIONS", value)
		assert.Equal(t, want, runMigrationsAtStartup(), "RUN_MIGRATIONS=%q", value)
	}
}
