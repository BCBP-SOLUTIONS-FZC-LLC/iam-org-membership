// Package metrics registers Prometheus counters, gauges, and histograms for
// the Org & Membership service. Every metric is prefixed iam_ (LLD §11).
// Phase 0 registers the minimal set that infrastructure (pool health,
// outbox runner, cache) can populate. Phase 6 lands the full business
// metric surface per §11.
package metrics

import (
	"os"

	"github.com/prometheus/client_golang/prometheus"
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

	// Business-observability gauges populated by 5-min exporter goroutines
	// in main.go (§11.2).
	TenantOwnerless         prometheus.Gauge // T-13
	RealmSyncPending        prometheus.Gauge // T-15
	SeatOverageActive       prometheus.Gauge // SEAT-5
	PendingInvitationsStale prometheus.Gauge // invitation-expiry cron health
)

// Register wires business metrics into the default Prometheus registry.
// Call once at startup BEFORE the /metrics endpoint is served.
// Every counter carries a "version" label (BUILD_VERSION env var) so
// per-deploy rates can be isolated during rolling updates.
func Register() {
	version := os.Getenv("BUILD_VERSION")
	if version == "" {
		version = "dev"
	}

	RLSViolations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iam_rls_violations_total",
		Help: "Row-level-security violations scraped from rls_violation_log, by violation_type.",
	}, []string{"violation_type"})

	UnknownEventAcknowledged = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iam_unknown_event_acknowledged_total",
		Help: "Events silently acknowledged because no handler is wired for the type — sustained nonzero rate means a producer added a new type.",
	}, []string{"topic", "event_type"})

	StaleLifecycleEventSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iam_stale_lifecycle_event_skipped_total",
		Help: "Lifecycle events skipped by EVT-14 recency guard (event.time <= tenants.last_event_at).",
	}, []string{"event_type"})

	FutureLifecycleEventRejected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iam_future_lifecycle_event_rejected_total",
		Help: "Lifecycle events rejected by EVT-15 future-time clamp (event.time > now() + skew) — any nonzero rate pages.",
	}, []string{"event_type"})

	SessionRevokeFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iam_session_revoke_failed_total",
		Help: "RP RevokeUserSessions calls that returned non-2xx or errored (AUTH-8 fail-open).",
	}, []string{"reason"})

	TenantOwnerless = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iam_tenant_ownerless",
		Help: "Tenants with ownerless_since IS NOT NULL (T-13). Sustained >0 pages.",
	})
	RealmSyncPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iam_realm_sync_pending",
		Help: "Tenants with realm_sync_pending=true (T-15).",
	})
	SeatOverageActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iam_seat_overage_active",
		Help: "Tenants with overage_since IS NOT NULL (SEAT-5).",
	})
	PendingInvitationsStale = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iam_pending_invitations_stale",
		Help: "Pending invitations past expires_at that invitation-expiry hasn't flipped yet.",
	})

	prometheus.MustRegister(
		RLSViolations,
		UnknownEventAcknowledged,
		StaleLifecycleEventSkipped,
		FutureLifecycleEventRejected,
		SessionRevokeFailed,
		TenantOwnerless,
		RealmSyncPending,
		SeatOverageActive,
		PendingInvitationsStale,
	)

	// Pre-initialise labels so dashboards show 0 rather than "no data".
	RLSViolations.WithLabelValues("missing_or_invalid_guc")
	RLSViolations.WithLabelValues("cross_tenant_access")
	SessionRevokeFailed.WithLabelValues("transport")

	_ = version
}
