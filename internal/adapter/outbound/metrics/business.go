// Package metrics registers Prometheus counters, gauges, and histograms for
// the Org & Membership service. Every metric is prefixed iam_ (LLD §11).
// Phase 0 registers the minimal set that infrastructure (pool health,
// outbox runner, cache) can populate. Phase 6 lands the full business
// metric surface per §11.
package metrics

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

var (
	// RLSViolations counts audit rows written by rls_violation_log — populated
	// by a 5-minute exporter goroutine started in main.go (LLD §11.2).
	// Labelled by violation_type so alerts can page separately on
	// cross_tenant_access (critical) vs missing_or_invalid_guc (warn).
	RLSViolations *prometheus.CounterVec

	// UnknownEventAcknowledged counts events consumed off tenant-orgm-q /
	// billing-orgm-q with a type this service does not handle (§6, event
	// consumer scope — silently ack + log + metric for forward-compat).
	// A sustained nonzero rate pages: add a handler for the surfacing type.
	UnknownEventAcknowledged *prometheus.CounterVec

	// StaleLifecycleEventSkipped counts events dropped by EVT-14 (§16 A33):
	// event.time <= tenants.last_event_at → projection unchanged, still
	// recorded in processed_events. Non-zero rate is normal under producer
	// reordering; sustained high rate indicates a consumer lag.
	StaleLifecycleEventSkipped *prometheus.CounterVec

	// FutureLifecycleEventRejected counts events rejected by EVT-15 (§16
	// A40): event.time > now() + MAX_LIFECYCLE_EVENT_SKEW_SECONDS →
	// DLQ, not recorded in processed_events. Any nonzero rate pages — a
	// producer's clock is skewed.
	FutureLifecycleEventRejected *prometheus.CounterVec

	// SessionRevokeFailed counts RP RevokeUserSessions calls that returned
	// non-2xx or errored (AUTH-8 fail-open).
	SessionRevokeFailed *prometheus.CounterVec

	// DelegateSuspendImpact counts P-7 suspensions where the user was a
	// delegate on active workflows and the WFI-13 advisory `delegate_impact`
	// warning fired (§16 C3, §8.8.5). Distinct from delegate_removal_blocked
	// — this is a non-fatal advisory, never a 409.
	DelegateSuspendImpact *prometheus.CounterVec

	// TenantOwnerlessEscalated counts the moment a removal drops the last
	// active tenant_owner. Event-time signal (distinct from the periodic
	// TenantOwnerless gauge, which is a scan). LLD §11.4 / TM-12 line 3807.
	TenantOwnerlessEscalated *prometheus.CounterVec

	// ── LLD §11.2 counters — every alert documented in the LLD needs a
	//    corresponding counter. Labels held to low cardinality per §16 A48.

	// SeatOverageStarted counts transitions from under-cap to over-cap
	// (SEAT-5). Distinct from the SeatOverageActive gauge (a scan).
	SeatOverageStarted *prometheus.CounterVec

	// SeatLimitReached counts P-6 invites blocked by SEAT-1 hitting cap.
	SeatLimitReached *prometheus.CounterVec

	// InviteThrottled counts P-6 invites rate-limited (§16 A41).
	InviteThrottled *prometheus.CounterVec

	// RealmSyncFailed counts realm-config-sync reconciler failures (T-15).
	RealmSyncFailed *prometheus.CounterVec

	// DelegateRemovalBlocked counts P-7 removals that returned 409
	// workflow_resolution_required (§8.8.1 WFI-3).
	DelegateRemovalBlocked *prometheus.CounterVec

	// DelegateReassignment counts P-26 successful `replace_delegate`
	// removals (§8.8, RemovalReplaceDelegate).
	DelegateReassignment *prometheus.CounterVec

	// ProcessedEventsDuplicates counts SQS redeliveries filtered by the
	// processed_events composite PK (IDEMP-4 / PE-1).
	ProcessedEventsDuplicates *prometheus.CounterVec

	// LifecycleConsumerLagSeconds is a histogram of (now - event.time) when
	// the consumer picks up a lifecycle event. Sustained high P99 flags
	// an SQS backlog or slow downstream apply.
	LifecycleConsumerLagSeconds *prometheus.HistogramVec

	// XsvcCallLatencySeconds times the three new synchronous cross-service
	// client calls (catalogadmin/groupmappingclient/delegationcheck) — LLD
	// §11.2, source for the §18.7-§18.9 latency budgets. Labelled by
	// service ("catalog"|"group_mapping"|"delegation") and endpoint.
	XsvcCallLatencySeconds *prometheus.HistogramVec

	// XsvcCallErrors counts cross-service call failures by outcome
	// ("5xx"|"timeout"|"fallback_served"). "fallback_served" is recorded by
	// the calling service (CatalogService/GroupMappingService), not the
	// client, since only the caller knows whether a stale/last-known-good
	// value was served instead of surfacing the error.
	XsvcCallErrors *prometheus.CounterVec

	// MembershipExistsCheck counts I-15 grant-time membership-existence
	// checks served, by caller and result (§11.2).
	MembershipExistsCheck *prometheus.CounterVec

	// Business-observability gauges populated by 5-min exporter goroutines
	// in main.go (§11.2).
	TenantOwnerless         prometheus.Gauge // T-13
	RealmSyncPending        prometheus.Gauge // T-15
	SeatOverageActive       prometheus.Gauge // SEAT-5
	PendingInvitationsStale prometheus.Gauge // invitation-expiry cron health
)

// ObserveXsvcLatency records a cross-service call's duration. Nil-safe —
// XsvcCallLatencySeconds is only non-nil once Register() has run (server
// startup), so client/service unit tests that never call Register() get a
// silent no-op rather than a nil-pointer panic.
func ObserveXsvcLatency(service, endpoint string, seconds float64) {
	if XsvcCallLatencySeconds != nil {
		XsvcCallLatencySeconds.WithLabelValues(service, endpoint).Observe(seconds)
	}
}

// IncXsvcError records a cross-service call failure by outcome. Nil-safe,
// see ObserveXsvcLatency.
func IncXsvcError(service, endpoint, outcome string) {
	if XsvcCallErrors != nil {
		XsvcCallErrors.WithLabelValues(service, endpoint, outcome).Inc()
	}
}

// IncMembershipExistsCheck records an I-15 grant-time membership-existence
// check. Nil-safe, see ObserveXsvcLatency.
func IncMembershipExistsCheck(caller, result string) {
	if MembershipExistsCheck != nil {
		MembershipExistsCheck.WithLabelValues(caller, result).Inc()
	}
}

// XsvcOutcome classifies a cross-service client transport error into one of
// iam_xsvc_call_errors_total's two client-observable outcomes ("timeout" |
// "5xx"). The third outcome, "fallback_served", is recorded by the calling
// service layer, not here — only it knows whether a stale/last-known-good
// value was served instead of surfacing the error.
func XsvcOutcome(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "5xx"
}

// gincommonLabels returns a copy of gincommon's {service, version} const
// labels so business collectors scrape on the same registry and labels as
// HTTP metrics. skip drops names that collide with a variable label (the
// iam_xsvc_* collectors already use "service" for the downstream peer).
// Returns nil when ObservabilityMiddlewares has not run yet, matching
// prometheus's "no const labels" zero value so unit tests that never
// bootstrap gincommon still register cleanly.
func gincommonLabels(skip ...string) prometheus.Labels {
	labels := gincommon.MetricsConstLabels()
	for _, k := range skip {
		delete(labels, k)
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}

var registerOnce sync.Once

// Register wires business metrics onto gincommon's Prometheus registerer
// (same registry and {service, version} const labels as HTTP metrics).
// Call once at startup AFTER ObservabilityMiddlewares has run and BEFORE
// the /metrics endpoint is served. Idempotent.
func Register() {
	registerOnce.Do(registerMetrics)
}

func registerMetrics() {
	labels := gincommonLabels()
	xsvcLabels := gincommonLabels("service")

	RLSViolations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_rls_violations_total",
		Help:        "Row-level-security violations scraped from rls_violation_log, by violation_type.",
		ConstLabels: labels,
	}, []string{"violation_type"})

	UnknownEventAcknowledged = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_unknown_event_acknowledged_total",
		Help:        "Events silently acknowledged because no handler is wired for the type — sustained nonzero rate means a producer added a new type.",
		ConstLabels: labels,
	}, []string{"topic", "event_type"})

	StaleLifecycleEventSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_stale_lifecycle_event_skipped_total",
		Help:        "Lifecycle events skipped by EVT-14 recency guard (event.time <= tenants.last_event_at).",
		ConstLabels: labels,
	}, []string{"event_type"})

	FutureLifecycleEventRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_future_lifecycle_event_rejected_total",
		Help:        "Lifecycle events rejected by EVT-15 future-time clamp (event.time > now() + skew) — any nonzero rate pages.",
		ConstLabels: labels,
	}, []string{"event_type"})

	SessionRevokeFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_session_revoke_failed_total",
		Help:        "RP RevokeUserSessions calls that returned non-2xx or errored (AUTH-8 fail-open).",
		ConstLabels: labels,
	}, []string{"reason"})

	DelegateSuspendImpact = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_delegate_suspend_impact_total",
		Help:        "P-7 suspensions where the user was a delegate on active workflows and the WFI-13 advisory fired (advisory, never a block).",
		ConstLabels: labels,
	}, []string{"checked"})

	TenantOwnerlessEscalated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_tenant_ownerless_escalated_total",
		Help:        "Count of tenants that just entered the ownerless state on this write (TM-12). Every increment should page.",
		ConstLabels: labels,
	}, []string{"reason"})

	SeatOverageStarted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_seat_overage_started_total",
		Help:        "Transitions from under-cap to over-cap on tenant seat consumption (SEAT-5).",
		ConstLabels: labels,
	}, []string{"cause"})

	SeatLimitReached = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_seat_limit_reached_total",
		Help:        "P-6 invite attempts blocked by SEAT-1 cap.",
		ConstLabels: labels,
	}, []string{"plan"})

	InviteThrottled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_invite_throttled_total",
		Help:        "P-6 invites rate-limited (§16 A41).",
		ConstLabels: labels,
	}, []string{"reason"})

	RealmSyncFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_realm_sync_failed_total",
		Help:        "realm-config-sync reconciler failures (T-15).",
		ConstLabels: labels,
	}, []string{"stage"})

	DelegateRemovalBlocked = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_delegate_removal_blocked_total",
		Help:        "P-7 removals blocked by WFI-3 delegate-impact pre-check (409 workflow_resolution_required).",
		ConstLabels: labels,
	}, []string{"scope"})

	DelegateReassignment = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_delegate_reassignment_total",
		Help:        "P-26 removal-resolution completions by action (replace_delegate | stop_workflows).",
		ConstLabels: labels,
	}, []string{"action"})

	ProcessedEventsDuplicates = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_processed_events_duplicates_total",
		Help:        "SQS redeliveries filtered by processed_events composite PK (IDEMP-4).",
		ConstLabels: labels,
	}, []string{"consumer"})

	LifecycleConsumerLagSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "iam_lifecycle_consumer_lag_seconds",
		Help:        "Seconds between event.time and consumer apply time. Sustained high P99 flags backlog.",
		Buckets:     []float64{0.05, 0.1, 0.5, 1, 5, 15, 60, 300, 1800},
		ConstLabels: labels,
	}, []string{"event_type"})

	XsvcCallLatencySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "iam_xsvc_call_latency_seconds",
		Help:        "Latency of synchronous cross-service client calls (catalog/group_mapping/delegation), by endpoint.",
		Buckets:     []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 3},
		ConstLabels: xsvcLabels,
	}, []string{"service", "endpoint"})

	XsvcCallErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_xsvc_call_errors_total",
		Help:        "Cross-service call failures by service/endpoint/outcome (5xx|timeout|fallback_served).",
		ConstLabels: xsvcLabels,
	}, []string{"service", "endpoint", "outcome"})

	MembershipExistsCheck = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_membership_exists_check_total",
		Help:        "I-15 grant-time membership-existence checks served, by caller and result.",
		ConstLabels: labels,
	}, []string{"caller", "result"})

	TenantOwnerless = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_tenant_ownerless",
		Help:        "Tenants with ownerless_since IS NOT NULL (T-13). Sustained >0 pages.",
		ConstLabels: labels,
	})
	RealmSyncPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_realm_sync_pending",
		Help:        "Tenants with realm_sync_pending=true (T-15).",
		ConstLabels: labels,
	})
	SeatOverageActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_seat_overage_active",
		Help:        "Tenants with overage_since IS NOT NULL (SEAT-5).",
		ConstLabels: labels,
	})
	PendingInvitationsStale = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_pending_invitations_stale",
		Help:        "Pending invitations past expires_at that invitation-expiry hasn't flipped yet.",
		ConstLabels: labels,
	})

	gincommon.MetricsRegisterer().MustRegister(
		RLSViolations,
		UnknownEventAcknowledged,
		StaleLifecycleEventSkipped,
		FutureLifecycleEventRejected,
		SessionRevokeFailed,
		DelegateSuspendImpact,
		TenantOwnerlessEscalated,
		SeatOverageStarted,
		SeatLimitReached,
		InviteThrottled,
		RealmSyncFailed,
		DelegateRemovalBlocked,
		DelegateReassignment,
		ProcessedEventsDuplicates,
		LifecycleConsumerLagSeconds,
		XsvcCallLatencySeconds,
		XsvcCallErrors,
		MembershipExistsCheck,
		TenantOwnerless,
		RealmSyncPending,
		SeatOverageActive,
		PendingInvitationsStale,
	)

	// Pre-initialise labels so dashboards show 0 rather than "no data".
	RLSViolations.WithLabelValues("missing_or_invalid_guc")
	RLSViolations.WithLabelValues("cross_tenant_access")
	SessionRevokeFailed.WithLabelValues("transport")
}
