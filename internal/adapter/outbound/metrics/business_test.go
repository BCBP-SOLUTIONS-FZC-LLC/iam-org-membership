package metrics

import (
	"errors"
	"net/http"
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
	// Call it before Register so collectors pick up {domain, service,
	// environment} const labels and land on gincommon's registerer — the
	// same order cmd/server/main.go uses.
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		ServiceName:  "iam-org-membership",
		BuildVersion: "test",
		Domain:       "iam",
		Environment:  "test",
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

	// Tier 1 — platform_* (registry-built, shared with platform-events).
	assert.NotNil(t, DuplicateMessages, "DuplicateMessages must be initialised")
	assert.NotNil(t, DLQMessages, "DLQMessages must be initialised")
	assert.NotNil(t, DependencyRequestSeconds, "DependencyRequestSeconds must be initialised")

	// Tier 3 predecessors (shadow-authoritative until ratification).
	assert.NotNil(t, DependencyCallDurationSeconds, "DependencyCallDurationSeconds must be initialised")
	assert.NotNil(t, DependencyCallFailuresTotal, "DependencyCallFailuresTotal must be initialised")
	assert.NotNil(t, ProcessedEventsDuplicatesTotal, "ProcessedEventsDuplicatesTotal must be initialised")

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
	DuplicateMessages.WithLabelValues("stability-check", "stability-check").Inc()
	DLQMessages.WithLabelValues("consume", "stability-check", "stability_check").Inc()
	DependencyRequestSeconds.WithLabelValues("stability-check", "stability_check", "success").Observe(0.1)
	DependencyCallDurationSeconds.WithLabelValues("stability-check", "stability-check").Observe(0.1)
	DependencyCallFailuresTotal.WithLabelValues("stability-check", "stability-check", "stability-check").Inc()
	ProcessedEventsDuplicatesTotal.WithLabelValues("stability-check").Inc()
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
		"platform_duplicate_messages_total",
		"platform_dlq_messages_total",
		"platform_dependency_request_seconds",
		// Tier 3 predecessors, shadow-authoritative until ratification.
		"iam_org_membership_dependency_call_duration_seconds",
		"iam_org_membership_dependency_call_failures_total",
		"iam_org_membership_processed_events_duplicates_total",
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
		"iam_lifecycle_event_skipped_total":                    "EVT-14",
		"platform_dlq_messages_total":                          "dead-letter",
		"iam_org_membership_seat_overage_started_total":        "SEAT-5",
		"iam_org_membership_seat_limit_reached_total":          "SEAT-1",
		"iam_org_membership_tenant_ownerless_escalated_total":  "TM-12",
		"iam_org_membership_processed_events_duplicates_total": "IDEMP-4",
		"iam_org_membership_realm_sync_pending":                "T-15",
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

	got := serviceLabels("ignored")
	assert.Equal(t, "iam-org-membership", got["service"])
	assert.Equal(t, "iam", got["domain"], "every collector carries the gincommon identity, domain included")
	assert.Equal(t, "test", got["environment"])
	_, hasVersion := got["version"]
	assert.False(t, hasVersion, "version lives only on platform_build_info")
}

// TestDependencyMetrics_RegistryLabels — platform_dependency_request_
// seconds uses the registry's {dependency, operation, outcome} labels (the
// downstream peer is "dependency", never "service", which is reserved for
// the emitter), while the Tier-3 histogram keeps {target_service,
// endpoint}.
func TestDependencyMetrics_RegistryLabels(t *testing.T) {
	ensureRegistered(t)

	ObserveDependencyCall("catalog", "group-resolution", 0.01, &http.Response{StatusCode: 503}, nil)
	ObserveDependencyCall("catalog", "plans", 0.01, &http.Response{StatusCode: 200}, nil)
	ObserveDependencyCall("catalog", "plans", 0.01, nil, errors.New("dial tcp: refused"))

	assert.Equal(t, 1, testutil.CollectAndCount(DependencyRequestSeconds.WithLabelValues("catalog", "group_resolution", "error").(prometheus.Collector)))
	h := DependencyRequestSeconds.WithLabelValues("catalog", "plans", "error").(prometheus.Histogram)
	var m dto.Metric
	require.NoError(t, h.Write(&m))
	assert.Equal(t, uint64(1), m.GetHistogram().GetSampleCount(), "transport error is outcome=error")
	ok := DependencyRequestSeconds.WithLabelValues("catalog", "plans", "success").(prometheus.Histogram)
	require.NoError(t, ok.Write(&m))
	assert.Equal(t, uint64(1), m.GetHistogram().GetSampleCount())
	assert.Positive(t, testutil.CollectAndCount(DependencyCallDurationSeconds), "Tier-3 latency histogram must record too")

	gathered, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range gathered {
		if mf.GetName() != "platform_dependency_request_seconds" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			assert.Equal(t, "iam", labels["domain"])
			_, hasTarget := labels["target_service"]
			assert.False(t, hasTarget, "registry shape has no target_service")
			assert.Contains(t, []string{"success", "error"}, labels["outcome"])
		}
	}
}

// TestIncDependencyError_Tier3Only — failures land on the authoritative
// iam_org_membership_dependency_call_failures_total; no unregistered
// platform_* shadow counter is emitted for them any more.
func TestIncDependencyError_Tier3Only(t *testing.T) {
	ensureRegistered(t)

	before := testutil.ToFloat64(DependencyCallFailuresTotal.WithLabelValues("delegation", "dual-emit-check", "timeout"))
	IncDependencyError("delegation", "dual-emit-check", "timeout")
	assert.Equal(t, before+1, testutil.ToFloat64(DependencyCallFailuresTotal.WithLabelValues("delegation", "dual-emit-check", "timeout")))

	gathered, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range gathered {
		if strings.HasPrefix(mf.GetName(), "platform_dependency_") {
			assert.Equal(t, "platform_dependency_request_seconds", mf.GetName(), "only the registry dependency metric may be emitted")
		}
	}
}

// TestPlatformRegistry_EveryEntryHasATier3Predecessor — the standard's
// governance rules (11/12) require a Proposed platform_* metric to stay
// non-authoritative; this repo's mechanism for that is dual-emission to a
// named Tier-3 predecessor. A registry entry without one would have no
// authoritative fallback for alerts/SLOs to point at.
func TestPlatformRegistry_EveryEntryHasATier3Predecessor(t *testing.T) {
	for _, entry := range PlatformRegistry {
		if entry.Status != StatusProposed {
			continue
		}
		assert.NotEmpty(t, entry.SupersedesTier3,
			"Proposed entry %q must name at least one Tier-3 predecessor", entry.Name)
	}
}
