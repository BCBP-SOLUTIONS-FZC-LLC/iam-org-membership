//go:build integration

// Package integration_test hosts cross-layer integration tests that spin up
// real Postgres + Valkey + LocalStack via testcontainers. Phase 3 populates
// this with EVT-14/15/16 recency/clamp/relay tests, outbox publish tests,
// and idempotency tests. Phase 1 leaves it as a compile-only placeholder.
package integration_test

import "testing"

func TestPlaceholder(t *testing.T) {
	t.Skip("Phase 3 lands the integration test suite (EVT-14/15/16, outbox, idempotency)")
}
