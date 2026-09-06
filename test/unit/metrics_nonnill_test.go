// metrics_nonnill_test.go covers the non-nil branches of the three
// nil-guarded metric helpers in internal/adapter/outbound/metrics/business.go.
//
// The existing tests in authz_supplement_test.go cover only the nil-guard
// path (when Register() has not yet been called). This file calls Register()
// first (idempotent via sync.Once) so the metric vars are non-nil, then
// exercises the guarded function bodies.
//
// Also covers the missing XsvcOutcome branch: errors.As matches a net.Error
// but Timeout() returns false → falls through to the "5xx" default.
package unit_test

import (
	"errors"
	"net"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/metrics"
	"github.com/stretchr/testify/assert"
)

// ensureMetricsRegisteredForUnit calls metrics.Register() at most once per
// test process. Register() is idempotent (sync.Once internally) so calling
// it from both the consumer package tests and here is safe.
func ensureMetricsRegisteredForUnit() {
	if metrics.XsvcCallLatencySeconds == nil {
		metrics.Register()
	}
}

// TestMetrics_ObserveXsvcLatency_NonNilMetric_NoPanic verifies that when
// XsvcCallLatencySeconds is non-nil (after Register()), ObserveXsvcLatency
// records the observation without panicking.
func TestMetrics_ObserveXsvcLatency_NonNilMetric_NoPanic(t *testing.T) {
	ensureMetricsRegisteredForUnit()

	assert.NotPanics(t, func() {
		metrics.ObserveXsvcLatency("catalog", "GET /internal/plans", 0.012)
	}, "ObserveXsvcLatency must not panic when XsvcCallLatencySeconds is registered")
}

// TestMetrics_IncXsvcError_NonNilMetric_NoPanic verifies that when
// XsvcCallErrors is non-nil (after Register()), IncXsvcError increments
// the counter without panicking.
func TestMetrics_IncXsvcError_NonNilMetric_NoPanic(t *testing.T) {
	ensureMetricsRegisteredForUnit()

	assert.NotPanics(t, func() {
		metrics.IncXsvcError("group_mapping", "POST /internal/tenants/{id}/group-resolution", "5xx")
	}, "IncXsvcError must not panic when XsvcCallErrors is registered")
}

// TestMetrics_IncMembershipExistsCheck_NonNilMetric_NoPanic verifies that
// when MembershipExistsCheck is non-nil (after Register()),
// IncMembershipExistsCheck increments without panicking.
func TestMetrics_IncMembershipExistsCheck_NonNilMetric_NoPanic(t *testing.T) {
	ensureMetricsRegisteredForUnit()

	assert.NotPanics(t, func() {
		metrics.IncMembershipExistsCheck("tender_acl", "active")
	}, "IncMembershipExistsCheck must not panic when MembershipExistsCheck is registered")
}

// TestMetrics_XsvcOutcome_NetErrorNotTimeout_Returns5xx covers the branch
// where errors.As(err, &netErr) matches a net.Error but Timeout() == false.
// In this case the function must fall through the first if and return "5xx"
// from the default branch.
//
// This is the 83.3% gap in XsvcOutcome: the condition
// `errors.As(err, &netErr) && netErr.Timeout()` evaluates to
// (true && false) = false, so neither "timeout" return fires.
func TestMetrics_XsvcOutcome_NetErrorNotTimeout_Returns5xx(t *testing.T) {
	// net.OpError implements net.Error; Timeout() reports whether the
	// underlying error timed out. A simple OpError with a non-timeout error
	// returns false from Timeout().
	nonTimeoutNetErr := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: errors.New("connection refused"),
	}
	// Verify the test fixture is correctly constructed.
	var netErr net.Error
	assert.True(t, errors.As(nonTimeoutNetErr, &netErr), "fixture must satisfy net.Error")
	assert.False(t, netErr.Timeout(), "fixture must NOT be a timeout error")

	outcome := metrics.XsvcOutcome(nonTimeoutNetErr)
	assert.Equal(t, "5xx", outcome,
		"net.Error where Timeout()=false must be classified as '5xx'")
}

// deadlineIsErr is a custom error type that satisfies errors.Is(_, context.DeadlineExceeded)
// via the Is() method but does NOT implement net.Error. This exercises the
// second "timeout" return in XsvcOutcome (lines 153-155) — the branch that is
// unreachable with context.DeadlineExceeded directly (which is also a net.Error
// with Timeout()=true, so it's caught by the first if).
type deadlineIsErr struct{}

func (e deadlineIsErr) Error() string { return "custom deadline exceeded" }
func (e deadlineIsErr) Is(target error) bool {
	// Intentionally only match context.DeadlineExceeded by value comparison,
	// without wrapping it (no Unwrap). This prevents errors.As from finding
	// a net.Error in the chain.
	return target.Error() == "context deadline exceeded"
}

// TestMetrics_XsvcOutcome_DeadlineExceeded_ViaIsMethod covers the second
// `return "timeout"` branch in XsvcOutcome (block 153.46,155.3). That branch
// is only reachable when errors.Is matches context.DeadlineExceeded but
// errors.As does NOT find a net.Error — i.e., the error satisfies Is() via a
// custom method without embedding the deadline error in the chain.
func TestMetrics_XsvcOutcome_DeadlineExceeded_ViaIsMethod(t *testing.T) {
	err := deadlineIsErr{}

	// Verify fixture properties.
	var netErr net.Error
	assert.False(t, errors.As(err, &netErr), "fixture must NOT satisfy net.Error")

	outcome := metrics.XsvcOutcome(err)
	assert.Equal(t, "timeout", outcome,
		"error matching context.DeadlineExceeded via Is() must be classified as 'timeout'")
}
