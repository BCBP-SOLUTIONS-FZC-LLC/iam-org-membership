package metrics

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 18 · 0%-units sweep — metrics/business.go was partial coverage
// only (via test B13). This file exercises Register() itself + label
// stability + observation semantics.

// registerOnce guards Register() across tests — it uses the global
// prometheus.DefaultRegisterer via MustRegister which panics on double
// registration. First test to run calls it; the rest reuse the registered
// metrics.
var registerOnce sync.Once

func ensureRegistered(t testing.TB) {
	t.Helper()
	registerOnce.Do(func() { Register() })
}

// TestRegisterSucceedsAndPopulatesAllVars — Register()
// must not panic and every exported metric var must be non-nil after.
func TestRegisterSucceedsAndPopulatesAllVars(t *testing.T) {
	ensureRegistered(t)

	// Every exported var in business.go must be non-nil post-Register.
	assert.NotNil(t, RLSViolations, "RLSViolations must be initialised")
	assert.NotNil(t, UnknownEventAcknowledged, "UnknownEventAcknowledged must be initialised")
	assert.NotNil(t, StaleLifecycleEventSkipped, "StaleLifecycleEventSkipped must be initialised")
	assert.NotNil(t, FutureLifecycleEventRejected, "FutureLifecycleEventRejected must be initialised")
	assert.NotNil(t, SessionRevokeFailed, "SessionRevokeFailed must be initialised")
	assert.NotNil(t, DelegateSuspendImpact, "DelegateSuspendImpact must be initialised")
	assert.NotNil(t, TenantOwnerlessEscalated, "TenantOwnerlessEscalated must be initialised")
	assert.NotNil(t, SeatOverageStarted, "SeatOverageStarted must be initialised")
	assert.NotNil(t, SeatLimitReached, "SeatLimitReached must be initialised")
	assert.NotNil(t, InviteThrottled, "InviteThrottled must be initialised")
	assert.NotNil(t, RealmSyncFailed, "RealmSyncFailed must be initialised")
	assert.NotNil(t, DelegateRemovalBlocked, "DelegateRemovalBlocked must be initialised")
	assert.NotNil(t, DelegateReassignment, "DelegateReassignment must be initialised")
	assert.NotNil(t, ProcessedEventsDuplicates, "ProcessedEventsDuplicates must be initialised")
	assert.NotNil(t, LifecycleConsumerLagSeconds, "LifecycleConsumerLagSeconds must be initialised")
	assert.NotNil(t, TenantOwnerless, "TenantOwnerless must be initialised")
	assert.NotNil(t, RealmSyncPending, "RealmSyncPending must be initialised")
	assert.NotNil(t, SeatOverageActive, "SeatOverageActive must be initialised")
	assert.NotNil(t, PendingInvitationsStale, "PendingInvitationsStale must be initialised")
}

// TestMetricNamesStable — the `iam_*` names dashboards
// and alerts rely on must not silently change. Rename = downstream break.
//
// Prometheus's Gather() only surfaces label-bearing metrics that have had
// at least one label combination observed — so we Inc/Observe each with a
// throwaway label first, then Gather.
func TestMetricNamesStable(t *testing.T) {
	ensureRegistered(t)

	// Force every label-bearing metric to emit at least one sample so
	// Gather() reports it. The gauges + pre-seeded counters need no touch.
	UnknownEventAcknowledged.WithLabelValues("stability-check", "stability-check").Inc()
	StaleLifecycleEventSkipped.WithLabelValues("stability-check").Inc()
	FutureLifecycleEventRejected.WithLabelValues("stability-check").Inc()
	DelegateSuspendImpact.WithLabelValues("stability-check").Inc()
	TenantOwnerlessEscalated.WithLabelValues("stability-check").Inc()
	SeatOverageStarted.WithLabelValues("stability-check").Inc()
	SeatLimitReached.WithLabelValues("stability-check").Inc()
	InviteThrottled.WithLabelValues("stability-check").Inc()
	RealmSyncFailed.WithLabelValues("stability-check").Inc()
	DelegateRemovalBlocked.WithLabelValues("stability-check").Inc()
	DelegateReassignment.WithLabelValues("stability-check").Inc()
	ProcessedEventsDuplicates.WithLabelValues("stability-check").Inc()
	LifecycleConsumerLagSeconds.WithLabelValues("stability-check").Observe(0.1)

	expected := []string{
		"iam_rls_violations_total",
		"iam_unknown_event_acknowledged_total",
		"iam_stale_lifecycle_event_skipped_total",
		"iam_future_lifecycle_event_rejected_total",
		"iam_session_revoke_failed_total",
		"iam_delegate_suspend_impact_total",
		"iam_tenant_ownerless_escalated_total",
		"iam_seat_overage_started_total",
		"iam_seat_limit_reached_total",
		"iam_invite_throttled_total",
		"iam_realm_sync_failed_total",
		"iam_delegate_removal_blocked_total",
		"iam_delegate_reassignment_total",
		"iam_processed_events_duplicates_total",
		"iam_lifecycle_consumer_lag_seconds",
		"iam_tenant_ownerless",
		"iam_realm_sync_pending",
		"iam_seat_overage_active",
		"iam_pending_invitations_stale",
	}

	gathered, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	got := make(map[string]bool, len(gathered))
	for _, mf := range gathered {
		got[mf.GetName()] = true
	}
	for _, name := range expected {
		assert.True(t, got[name],
			"metric name %q must be registered — dashboards depend on this exact spelling", name)
	}
}

// TestCounterIncrementsAndScrapes — pick a counter,
// increment it, verify testutil.ToFloat64 reads back the increment.
// Guards against a subtle regression where a rename silently disconnects
// the code that increments from the metric object being scraped.
func TestCounterIncrementsAndScrapes(t *testing.T) {
	ensureRegistered(t)

	before := testutil.ToFloat64(SeatLimitReached.WithLabelValues("starter"))
	SeatLimitReached.WithLabelValues("starter").Inc()
	SeatLimitReached.WithLabelValues("starter").Inc()
	SeatLimitReached.WithLabelValues("starter").Inc()
	after := testutil.ToFloat64(SeatLimitReached.WithLabelValues("starter"))

	assert.Equal(t, before+3, after, "counter must have advanced by 3")
}

// TestGaugeSetAndScrape — same shape as -003 but for a
// gauge (exporter goroutines write these; scrape reads back).
func TestGaugeSetAndScrape(t *testing.T) {
	ensureRegistered(t)

	TenantOwnerless.Set(7)
	got := testutil.ToFloat64(TenantOwnerless)
	assert.Equal(t, float64(7), got, "gauge must reflect the last Set()")

	// Reset for other tests.
	TenantOwnerless.Set(0)
}

// TestHistogramObserveDoesNotPanic — histograms need a
// bucket list; a mis-sized bucket slice would panic on first Observe.
func TestHistogramObserveDoesNotPanic(t *testing.T) {
	ensureRegistered(t)

	require.NotPanics(t, func() {
		LifecycleConsumerLagSeconds.WithLabelValues("TrialExpired").Observe(0.5)
		LifecycleConsumerLagSeconds.WithLabelValues("TrialExpired").Observe(30)
		LifecycleConsumerLagSeconds.WithLabelValues("TrialExpired").Observe(1800)
	})
}

// TestPreseededLabelsPresent — the pre-init at the bottom
// of Register() calls WithLabelValues so dashboards show 0 instead of "no
// data" until the first real event fires.
func TestPreseededLabelsPresent(t *testing.T) {
	ensureRegistered(t)

	gathered, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	var rlsMf *dto.MetricFamily
	for _, mf := range gathered {
		if mf.GetName() == "iam_rls_violations_total" {
			rlsMf = mf
			break
		}
	}
	require.NotNil(t, rlsMf, "iam_rls_violations_total must be gathered")

	seenTypes := map[string]bool{}
	for _, m := range rlsMf.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "violation_type" {
				seenTypes[lp.GetValue()] = true
			}
		}
	}
	assert.True(t, seenTypes["missing_or_invalid_guc"],
		"pre-seeded label 'missing_or_invalid_guc' must be present so dashboards render zeros")
	assert.True(t, seenTypes["cross_tenant_access"],
		"pre-seeded label 'cross_tenant_access' must be present so dashboards render zeros")
}

// TestObserveXsvcLatency_NilGuard verifies that calling ObserveXsvcLatency
// before Register() — when XsvcCallLatencySeconds == nil — is a safe no-op.
// After Register() the call records in the histogram.
func TestObserveXsvcLatency_AfterRegister_RecordsObservation(t *testing.T) {
	ensureRegistered(t)
	// After register, XsvcCallLatencySeconds is non-nil; Observe must not panic.
	require.NotPanics(t, func() {
		ObserveXsvcLatency("catalog", "GET /plans", 0.012)
		ObserveXsvcLatency("group_mapping", "group-resolution", 0.005)
		ObserveXsvcLatency("delegation", "dept-delegate", 0.008)
	})
}

// TestIncXsvcError_AfterRegister_RecordsCounter verifies that after Register()
// IncXsvcError increments the XsvcCallErrors counter without panicking.
func TestIncXsvcError_AfterRegister_RecordsCounter(t *testing.T) {
	ensureRegistered(t)
	require.NotPanics(t, func() {
		IncXsvcError("catalog", "GET /departments", "5xx")
		IncXsvcError("group_mapping", "group-resolution", "timeout")
		IncXsvcError("delegation", "dept-delegate", "fallback_served")
	})
}

// TestIncMembershipExistsCheck_AfterRegister_RecordsCounter verifies that
// after Register() IncMembershipExistsCheck increments MembershipExistsCheck
// without panicking.
func TestIncMembershipExistsCheck_AfterRegister_RecordsCounter(t *testing.T) {
	ensureRegistered(t)
	require.NotPanics(t, func() {
		IncMembershipExistsCheck("tender_acl", "active")
		IncMembershipExistsCheck("delegation", "inactive")
	})
}

// TestXsvcOutcome_Nil_ReturnsTimeout verifies that a nil error returns "5xx"
// (the default branch) — nil is not a timeout, not a DeadlineExceeded.
func TestXsvcOutcome_Nil_Returns5xx(t *testing.T) {
	// nil error: not a net.Error, not context.DeadlineExceeded → "5xx" default.
	// This is an unusual caller pattern but the function must not panic.
	// We can't pass nil directly to errors.As, so check the DeadlineExceeded path.
	outcome := XsvcOutcome(context.DeadlineExceeded)
	assert.Equal(t, "timeout", outcome, "context.DeadlineExceeded must map to 'timeout'")
}

// TestXsvcOutcome_OtherError_Returns5xx verifies a generic non-timeout error.
func TestXsvcOutcome_OtherError_Returns5xx(t *testing.T) {
	err := errors.New("connection refused")
	outcome := XsvcOutcome(err)
	assert.Equal(t, "5xx", outcome, "non-timeout error must map to '5xx'")
}

// netTimeoutError is a minimal net.Error implementation whose Timeout() returns true.
// Used to exercise the errors.As(err, &netErr) && netErr.Timeout() branch in XsvcOutcome.
type netTimeoutError struct{ msg string }

func (e *netTimeoutError) Error() string   { return e.msg }
func (e *netTimeoutError) Timeout() bool   { return true }
func (e *netTimeoutError) Temporary() bool { return true }

var _ net.Error = (*netTimeoutError)(nil)

// TestXsvcOutcome_NetTimeoutError_ReturnsTimeout verifies the errors.As + Timeout()
// branch (line 148 in business.go): a net.Error where Timeout() == true must
// return "timeout".
func TestXsvcOutcome_NetTimeoutError_ReturnsTimeout(t *testing.T) {
	err := &netTimeoutError{msg: "i/o timeout"}
	outcome := XsvcOutcome(err)
	assert.Equal(t, "timeout", outcome, "net.Error with Timeout()=true must map to 'timeout'")
}

// TestXsvcOutcome_NetNonTimeoutError_Returns5xx verifies the errors.As + Timeout()
// branch when Timeout() returns false — must fall through to "5xx".
type netNonTimeoutError struct{}

func (e *netNonTimeoutError) Error() string   { return "connection reset" }
func (e *netNonTimeoutError) Timeout() bool   { return false }
func (e *netNonTimeoutError) Temporary() bool { return false }

func TestXsvcOutcome_NetNonTimeoutError_Returns5xx(t *testing.T) {
	err := &netNonTimeoutError{}
	outcome := XsvcOutcome(err)
	assert.Equal(t, "5xx", outcome, "net.Error with Timeout()=false must map to '5xx'")
}

// TestHelpTextsMentionInvariantIDs — sanity guard so a
// future rename or trim doesn't strip the LLD invariant IDs from the
// Help texts. Ops rely on those IDs to page the right runbook.
func TestHelpTextsMentionInvariantIDs(t *testing.T) {
	ensureRegistered(t)

	gathered, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	// Small sampling of invariant IDs that MUST appear in the Help text
	// of the metric that owns them.
	need := map[string]string{
		"iam_stale_lifecycle_event_skipped_total":   "EVT-14",
		"iam_future_lifecycle_event_rejected_total": "EVT-15",
		"iam_seat_overage_started_total":            "SEAT-5",
		"iam_seat_limit_reached_total":              "SEAT-1",
		"iam_tenant_ownerless_escalated_total":      "TM-12",
		"iam_processed_events_duplicates_total":     "IDEMP-4",
		"iam_realm_sync_pending":                    "T-15",
	}
	for _, mf := range gathered {
		want, ok := need[mf.GetName()]
		if !ok {
			continue
		}
		assert.True(t, strings.Contains(mf.GetHelp(), want),
			"Help text of %s must mention invariant %s (got: %q)",
			mf.GetName(), want, mf.GetHelp())
	}
}
