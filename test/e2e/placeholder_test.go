//go:build e2e

// Package e2e_test hosts end-to-end tests that hit the running server via
// HTTP against a fully-provisioned dev stack. Phase 4+ populates this with
// the trial-signup, invite/accept, and delegation flows. Phase 1 placeholder.
package e2e_test

import "testing"

func TestPlaceholder(t *testing.T) {
	t.Skip("Phase 4+ lands the e2e test suite")
}
