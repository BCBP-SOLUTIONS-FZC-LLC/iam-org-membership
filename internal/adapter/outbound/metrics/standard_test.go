package metrics

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgmetrics"
)

// TestStandard_LibrariesShareRegistryCollectors runs the production
// registration order (gincommon → Register → events.InitMetrics →
// pgmetrics.InitWithIdentity, see cmd/server/main.go initLibraryMetrics)
// and asserts that platform-events adopts the registry-built
// platform_dlq_messages_total / platform_duplicate_messages_total /
// platform_dependency_request_seconds collectors instead of being refused
// (a refusal would disable the library's outbox and SQS dead-letter
// counting). With METRICS_SCRAPE_OUT set it writes the full scrape for
// `metricslint check`.
func TestStandard_LibrariesShareRegistryCollectors(t *testing.T) {
	ensureRegistered(t)

	identity := gincommon.MetricsConstLabels()
	evWarnings, err := events.InitMetrics(events.MetricsIdentityFromLabels(identity), gincommon.MetricsRegisterer())
	require.NoError(t, err)
	for _, w := range evWarnings {
		assert.NotContains(t, []string{
			"platform_dlq_messages_total",
			"platform_duplicate_messages_total",
			"platform_dependency_request_seconds",
		}, w.Metric, "platform-events must reuse the service's registry collector: %v", w.Err)
	}
	assert.Empty(t, evWarnings, "no platform-events metric may be disabled")

	pgWarnings, err := pgmetrics.InitWithIdentity(pgmetrics.IdentityFromLabels(identity), gincommon.MetricsRegisterer())
	require.NoError(t, err)
	assert.Empty(t, pgWarnings, "no platform-pgcommon metric may be disabled")

	// platform-events counts every DLQ forward on the shared collector; the
	// service counts the reject reason on its own Tier-3 counter.
	DLQMessages.WithLabelValues("consume", "TenantSuspended", "explicit").Inc()
	IncConsumedEventRejected("TenantSuspended", "schema_violation")
	exerciseEveryCollector(t)

	gathered, err := gincommon.MetricsGatherer().Gather()
	require.NoError(t, err)
	names := map[string]bool{}
	for _, mf := range gathered {
		names[mf.GetName()] = true
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "version" && (strings.HasPrefix(mf.GetName(), "platform_") || strings.HasPrefix(mf.GetName(), "iam_")) {
					assert.Equal(t, "platform_build_info", mf.GetName(), "version label only on platform_build_info")
				}
			}
		}
	}
	for _, want := range []string{"platform_build_info", "platform_dlq_messages_total", "platform_library_info", "iam_org_membership_consumed_event_rejected_total"} {
		assert.True(t, names[want], "%s must be exported", want)
	}

	if out := os.Getenv("METRICS_SCRAPE_OUT"); out != "" {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
		req.Header.Set("Accept", "text/plain")
		gincommon.MetricsHandler().ServeHTTP(rec, req) // the production /metrics handler
		require.NoError(t, os.WriteFile(out, rec.Body.Bytes(), 0o600))
	}
}

// exerciseEveryCollector records one sample with representative production
// label values on every collector this package owns, so the scrape written
// for `metricslint check` exposes every label set (an unsampled vec has no
// series, and its label values would go unchecked).
func exerciseEveryCollector(t *testing.T) {
	t.Helper()
	ObserveDependencyLatency("catalog", "plans", 0.01)
	for _, reason := range []string{
		DependencyReasonTimeout, DependencyReasonTransport, DependencyReasonFallbackServed,
		HTTPStatusReason(404), HTTPStatusReason(503), HTTPStatusReason(302),
	} {
		IncDependencyError("realm_provisioner", "revoke_user_sessions", reason)
	}
	IncDuplicate(t.Context(), "membership", "TenantSuspended")
	ObserveLifecycleEventLag("TenantSuspended", 0.2)
	LifecycleEventSkipped.WithLabelValues("TenantSuspended").Inc()
	UnknownEventAcknowledged.WithLabelValues("SomethingNew").Inc()
	DelegateSuspendImpact.WithLabelValues("true").Inc()
	TenantOwnerlessEscalated.WithLabelValues("user_removed").Inc()
	IncSeatOverageStarted(SeatOverageCauseReconciler)
	IncSeatLimitReached("trial")
	IncInviteThrottled(InviteThrottleReinviteTooSoon)
	IncRealmSyncFailed("patch_realm_config")
	IncDelegateRemovalBlocked(DelegateBlockScopeTenantRemoval)
	IncDelegateReassignment("replace_delegate")
	IncMembershipExistsCheck("tender_acl", "active")
	IncPublicTenantResolve("found")
	IncPlanChangeCheck("allowed")
}
