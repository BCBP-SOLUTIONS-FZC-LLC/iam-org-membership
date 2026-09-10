package metrics

import (
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

func TestMain(m *testing.M) {
	// ObservabilityMiddlewares is gincommon's public metrics-init API.
	// Call it before Register so collectors pick up {service, version}
	// const labels and land on gincommon's registerer — the same order
	// cmd/server/main.go uses.
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		ServiceName:  "iam-org-membership",
		BuildVersion: "test",
	})
	os.Exit(m.Run())
}

// Phase 18 · 0%-units sweep — metrics/business.go was partial coverage
// only (via test B13). This file exercises Register itself + label
// stability + observation semantics, reclassified for the IAM Platform
// Observability Standard's three-tier hierarchy (platform_*/iam_*/
// iam_org_membership_*).

// ensureRegistered calls Register("test") once per test process. Register
// is itself idempotent (sync.Once in business.go).
func ensureRegistered(t testing.TB) {
	t.Helper()
	Register("test")
}

// TestRegisterSucceedsAndPopulatesAllVars — Register must not panic and
// every exported metric var must be non-nil after.
func TestRegisterSucceedsAndPopulatesAllVars(t *testing.T) {
	ensureRegistered(t)

	// Tier 1 — platform_*.
	assert.NotNil(t, MessagesReceived, "MessagesReceived must be initialised")
	assert.NotNil(t, MessagesProcessed, "MessagesProcessed must be initialised")
	assert.NotNil(t, MessagesFailed, "MessagesFailed must be initialised")
	assert.NotNil(t, DuplicateMessages, "DuplicateMessages must be initialised")
	assert.NotNil(t, DLQMessages, "DLQMessages must be initialised")
	assert.NotNil(t, DependencyRequestSeconds, "DependencyRequestSeconds must be initialised")
	assert.NotNil(t, DependencyErrors, "DependencyErrors must be initialised")

	// Tier 2 — iam_*.
	assert.NotNil(t, RLSViolations, "RLSViolations must be initialised")
	assert.NotNil(t, AuthSessionRevokeFailed, "AuthSessionRevokeFailed must be initialised")
	assert.NotNil(t, LifecycleEventSkipped, "LifecycleEventSkipped must be initialised")
	assert.NotNil(t, LifecycleEventLagSeconds, "LifecycleEventLagSeconds must be initialised")

	// Tier 3 — iam_org_membership_*.
	assert.NotNil(t, UnknownEventAcknowledged, "UnknownEventAcknowledged must be initialised")
	assert.NotNil(t, DelegateSuspendImpact, "DelegateSuspendImpact must be initialised")
	assert.NotNil(t, TenantOwnerlessEscalated, "TenantOwnerlessEscalated must be initialised")
	assert.NotNil(t, SeatOverageStarted, "SeatOverageStarted must be initialised")
	assert.NotNil(t, SeatLimitReached, "SeatLimitReached must be initialised")
	assert.NotNil(t, InviteThrottled, "InviteThrottled must be initialised")
	assert.NotNil(t, RealmSyncFailed, "RealmSyncFailed must be initialised")
	assert.NotNil(t, DelegateRemovalBlocked, "DelegateRemovalBlocked must be initialised")
	assert.NotNil(t, DelegateReassignment, "DelegateReassignment must be initialised")
	assert.NotNil(t, MembershipExistsCheck, "MembershipExistsCheck must be initialised")
	assert.NotNil(t, TenantOwnerless, "TenantOwnerless must be initialised")
	assert.NotNil(t, RealmSyncPending, "RealmSyncPending must be initialised")
	assert.NotNil(t, SeatOverageActive, "SeatOverageActive must be initialised")
	assert.NotNil(t, PendingInvitationsStale, "PendingInvitationsStale must be initialised")
}

// TestMetricNamesStable — the exact names dashboards and alerts rely on
// must not silently change. Rename = downstream break.
//
// Prometheus's Gather() only surfaces label-bearing metrics that have had
// at least one label combination observed — so we Inc/Observe each with a
// throwaway label first, then Gather.
func TestMetricNamesStable(t *testing.T) {
	ensureRegistered(t)

	// Force every label-bearing metric to emit at least one sample so
	// Gather() reports it. The gauges + pre-seeded counters need no touch.
	MessagesReceived.WithLabelValues("stability-check").Inc()
	MessagesProcessed.WithLabelValues("stability-check").Inc()
	MessagesFailed.WithLabelValues("stability-check").Inc()
	DuplicateMessages.WithLabelValues("stability-check").Inc()
	DLQMessages.WithLabelValues("stability-check", "stability-check").Inc()
	DependencyRequestSeconds.WithLabelValues("stability-check", "stability-check").Observe(0.1)
	DependencyErrors.WithLabelValues("stability-check", "stability-check", "stability-check").Inc()
	UnknownEventAcknowledged.WithLabelValues("stability-check", "stability-check").Inc()
	LifecycleEventSkipped.WithLabelValues("stability-check").Inc()
	LifecycleEventLagSeconds.WithLabelValues("stability-check").Observe(0.1)
	AuthSessionRevokeFailed.WithLabelValues("stability-check").Inc()
	DelegateSuspendImpact.WithLabelValues("stability-check").Inc()
	TenantOwnerlessEscalated.WithLabelValues("stability-check").Inc()
	SeatOverageStarted.WithLabelValues("stability-check").Inc()
	SeatLimitReached.WithLabelValues("stability-check").Inc()
	InviteThrottled.WithLabelValues("stability-check").Inc()
	RealmSyncFailed.WithLabelValues("stability-check").Inc()
	DelegateRemovalBlocked.WithLabelValues("stability-check").Inc()
	DelegateReassignment.WithLabelValues("stability-check").Inc()

	expected := []string{
		// Tier 1 — platform_* (shared across domains).
		"platform_messages_received_total",
		"platform_messages_processed_total",
		"platform_messages_failed_total",
		"platform_duplicate_messages_total",
		"platform_dlq_messages_total",
		"platform_dependency_request_seconds",
		"platform_dependency_errors_total",
		// Tier 2 — iam_* (shared across IAM-domain services).
		"iam_rls_violations_total",
		"iam_auth_session_revoke_failed_total",
		"iam_lifecycle_event_skipped_total",
		"iam_lifecycle_event_lag_seconds",
		// Tier 3 — iam_org_membership_* (unique to this service).
		"iam_org_membership_unknown_event_acknowledged_total",
		"iam_org_membership_delegate_suspend_impact_total",
		"iam_org_membership_tenant_ownerless_escalated_total",
		"iam_org_membership_seat_overage_started_total",
		"iam_org_membership_seat_limit_reached_total",
		"iam_org_membership_invite_throttled_total",
		"iam_org_membership_realm_sync_failed_total",
		"iam_org_membership_delegate_removal_blocked_total",
		"iam_org_membership_delegate_reassignment_total",
		"iam_org_membership_tenant_ownerless",
		"iam_org_membership_realm_sync_pending",
		"iam_org_membership_seat_overage_active",
		"iam_org_membership_pending_invitations_stale",
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

	// Every counter ends in _total, every histogram ends in _seconds
	// (naming rules 4/5) — enforced here as a regression guard, not just
	// the CI naming-convention script.
	for _, mf := range gathered {
		name := mf.GetName()
		if !strings.HasPrefix(name, "platform_") && !strings.HasPrefix(name, "iam_") {
			continue
		}
		switch mf.GetType().String() {
		case "COUNTER":
			assert.True(t, strings.HasSuffix(name, "_total"), "counter %q must end in _total", name)
		case "HISTOGRAM":
			assert.True(t, strings.HasSuffix(name, "_seconds"), "histogram %q must end in _seconds", name)
		}
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
		LifecycleEventLagSeconds.WithLabelValues("TrialExpired").Observe(0.5)
		LifecycleEventLagSeconds.WithLabelValues("TrialExpired").Observe(30)
		LifecycleEventLagSeconds.WithLabelValues("TrialExpired").Observe(1800)
	})
}

// TestPreseededLabelsPresent — the pre-init at the bottom
// of Register calls WithLabelValues so dashboards show 0 instead of "no
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
		"iam_lifecycle_event_skipped_total":                   "EVT-14",
		"platform_dlq_messages_total":                         "pages",
		"iam_org_membership_seat_overage_started_total":       "SEAT-5",
		"iam_org_membership_seat_limit_reached_total":         "SEAT-1",
		"iam_org_membership_tenant_ownerless_escalated_total": "TM-12",
		"platform_duplicate_messages_total":                   "IDEMP-4",
		"iam_org_membership_realm_sync_pending":               "T-15",
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

func TestConstLabels_MatchObservabilityConstLabels(t *testing.T) {
	ensureRegistered(t)

	got := serviceLabels("test")
	assert.Equal(t, "iam-org-membership", got["service"])
	assert.Equal(t, "test", got["version"])
	assert.Equal(t, "test", got["environment"])
	_, hasDomain := got["domain"]
	assert.False(t, hasDomain, "Tier 2/3 collectors must not carry a 'domain' label — the iam_ namespace already encodes it")

	p := platformLabels("test")
	assert.Equal(t, "iam", p["domain"], "Tier 1 collectors must carry domain=iam")
	assert.Equal(t, "iam-org-membership", p["service"])
	assert.Equal(t, "test", p["environment"])
}

// TestDependencyMetrics_CarryServiceConstLabel_AndTargetServiceVariableLabel —
// IAM Platform Observability Standard: the emitting service is the
// "service" const label (shared with every other collector in this
// package); the downstream peer being called is the "target_service"
// variable label, never "service" — that name is reserved platform-wide
// for "who emitted this metric". Also asserts the Tier-1 domain="iam"
// const label is present.
func TestDependencyMetrics_CarryServiceConstLabel_AndTargetServiceVariableLabel(t *testing.T) {
	ensureRegistered(t)

	ObserveDependencyLatency("catalog", "GetPlans", 0.01)
	IncDependencyError("catalog", "GetPlans", "5xx")

	gathered, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	for _, mf := range gathered {
		if mf.GetName() != "platform_dependency_request_seconds" && mf.GetName() != "platform_dependency_errors_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labelNames := make(map[string]string, len(m.GetLabel()))
			for _, lp := range m.GetLabel() {
				labelNames[lp.GetName()] = lp.GetValue()
			}
			assert.Equal(t, "iam-org-membership", labelNames["service"],
				"%s must carry the emitting service as its 'service' const label", mf.GetName())
			assert.Equal(t, "iam", labelNames["domain"],
				"%s must carry domain=iam (Tier 1 required label)", mf.GetName())
			_, hasTarget := labelNames["target_service"]
			assert.True(t, hasTarget,
				"%s must label the downstream peer as 'target_service', not 'service'", mf.GetName())
		}
	}
}
