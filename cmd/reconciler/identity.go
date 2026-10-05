package main

import (
	"os"
	"strings"
)

// resolveAppEnv returns the one environment name this process uses for
// everything: metrics, tracing, logging and its own dev/prod checks. It uses
// the same precedence the platform libraries use on their own (APP_ENV, then
// ENVIRONMENT; trimmed and lowercased), defaulting to "dev". Passing the
// result explicitly to gincommon (Config.Environment / TracingConfig) keeps
// metrics and traces from resolving it separately and disagreeing — with
// APP_ENV unset the service used to default to "dev" while gincommon fell
// back to ENVIRONMENT or production defaults.
func resolveAppEnv() string {
	for _, key := range []string{"APP_ENV", "ENVIRONMENT"} {
		if v := strings.ToLower(strings.TrimSpace(os.Getenv(key))); v != "" {
			return v
		}
	}
	return "dev"
}

// loggerEnv maps appEnv onto the value gincommon's logger understands.
// gincommon treats only "dev"/"local" as development, so "test" (dev-like
// here: CI and local test runs) would otherwise get production JSON logs.
func loggerEnv(appEnv string) string {
	if isDevLikeEnv(appEnv) {
		return "dev"
	}
	return appEnv
}
