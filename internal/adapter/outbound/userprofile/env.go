package userprofile

import (
	"os"
	"strconv"
	"time"
)

// getenv reads an env var with a fallback. Shared between the three
// outbound-HTTP packages via package-local duplication (keeps them
// dependency-free of a shared "config" helper).
func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}
