// Package metrics registers Prometheus counters, gauges, and histograms for
// the Org & Membership service, per the IAM Platform Observability
// Standard's three-tier hierarchy:
//
//   - Tier 1 — platform_* : a concept common across MULTIPLE DOMAINS
//     (IAM, Workflow, Billing, Tender Management, ...) with identical
//     semantics — message-consumption lifecycle, cross-service dependency
//     calls. Carries no domain or service name in the metric itself;
//     "domain", "service", "environment" are injected centrally as
//     ConstLabels (below), never left to the call site.
//   - Tier 2 — iam_* : a concept shared across multiple services WITHIN
//     the IAM domain (RLS violations, session revocation, lifecycle-event
//     bookkeeping) but not meaningful outside it. Carries no service name;
//     "service", "environment" are injected centrally.
//   - Tier 3 — iam_org_membership_* : behavior unique to this one
//     service (seat caps, tenant-ownerless escalation, invitation
//     lifecycle, delegate-impact advisories, ...). No other IAM service
//     has an equivalent concept, so the service identity is baked into
//     the name itself rather than relied upon as a label.
//
// Every counter ends in _total; every histogram ends in _seconds. The
// "domain"/"service"/"environment" labels are never left to individual
// Inc()/Observe() call sites to supply — they're baked in once here via
// ConstLabels (platformLabels/serviceLabels), so instrumentation callers
// cannot omit or misspell them (rule 8).
//
// Phase 0 registered the minimal infra set; Phase 6 landed the full
// business metric surface per §11. This file was last reclassified to
// match the tiered platform_*/iam_*/iam_org_membership_* standard —
// see each var's doc comment for its tier and, for tier-1/2 promotions
// without a named example in the standard, the rationale.
package metrics

import (
	"context"
	"errors"
	"maps"
	"net"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// domain is this service's fixed IAM Platform Observability Standard
// domain identity — a constant, not a runtime-varying label value, since
// every metric this package registers is emitted by an IAM-domain service.
const domain = "iam"

var (
	// ── Tier 1 — platform_* (shared across domains) ─────────────────────

	// MessagesReceived/Processed/Failed count every inbound SQS message
	// this service consumes, labelled by queue (tenant-orgm-q |
	// billing-orgm-q). Wired centrally in cmd/server/main.go around
	// MembershipEventConsumer.Handle, not inside Handle itself, so the
	// queue identity — known only at the subscription call site — never
	// has to be threaded through consumer internals.
	MessagesReceived  *prometheus.CounterVec
	MessagesProcessed *prometheus.CounterVec
	MessagesFailed    *prometheus.CounterVec

	// DuplicateMessages counts SQS redeliveries filtered by the
	// processed_events composite PK (IDEMP-4 / PE-1) — the standard's
	// platform_duplicate_messages_total concept: message-dedup is a
	// generic queue-consumer semantic, not an IAM-specific one.
	DuplicateMessages *prometheus.CounterVec

	// DLQMessages counts events this service actively rejects to DLQ
	// without recording processed_events. Currently the sole reason is
	// EVT-15's future-time clamp (§16 A40: event.time > now() + skew,
	// a poison-pill/producer-clock-skew guard) — reason is still a label,
	// not baked into the name, so a second DLQ cause can be added later
	// without a new metric. Any nonzero rate pages (producer clock skew).
	DLQMessages *prometheus.CounterVec

	// DependencyRequestSeconds / DependencyErrors time and count the
	// three synchronous cross-service client calls (catalogadmin /
	// groupmappingclient / delegationcheck) — LLD §11.2, source for the
	// §18.7-§18.9 latency budgets. The standard's platform_dependency_
	// request_seconds concept verbatim: any IAM/Workflow/Billing/Tender
	// service making a synchronous outbound call to a peer can emit the
	// same shape. target_service (catalog|group_mapping|delegation) is
	// the downstream peer being called — distinct from the "service"
	// const label, which is always this metric's own emitting service.
	// DependencyErrors' outcome is 5xx|timeout|fallback_served;
	// "fallback_served" is recorded by the calling service
	// (CatalogService/GroupMappingService), not the client, since only
	// the caller knows whether a stale/last-known-good value was served
	// instead of surfacing the error. DependencyErrors itself has no
	// directly-named platform_* counterpart in the standard's examples —
	// added by symmetry with DependencyRequestSeconds (rule 7: prefer
	// shared whenever reusable) since outcome-classified failure counts
	// aren't recoverable from a latency histogram's buckets alone.
	DependencyRequestSeconds *prometheus.HistogramVec
	DependencyErrors         *prometheus.CounterVec

	// ── Tier 2 — iam_* (shared across IAM-domain services) ──────────────

	// RLSViolations counts audit rows written by rls_violation_log —
	// populated by a 5-minute exporter goroutine started in main.go (LLD
	// §11.2). Every IAM service on the shared Postgres RLS pattern
	// (FORCE ROW LEVEL SECURITY + tenant_isolation policy) can emit this
	// with identical semantics, so it stays domain-shared rather than
	// service-specific. Labelled by violation_type so alerts can page
	// separately on cross_tenant_access (critical) vs
	// missing_or_invalid_guc (warn).
	RLSViolations *prometheus.CounterVec

	// AuthSessionRevokeFailed counts RP RevokeUserSessions calls that
	// returned non-2xx or errored (AUTH-8 fail-open). AUTH-* is a shared
	// IAM-subsystem security-invariant catalogue, not unique to this
	// service's own business logic — any IAM service that reduces a
	// user's privilege via a Realm-Provisioner session-revoke call can
	// emit this identically, so it's grouped under the iam_auth_ prefix
	// rather than iam_org_membership_.
	AuthSessionRevokeFailed *prometheus.CounterVec

	// LifecycleEventSkipped / LifecycleEventLagSeconds cover the EVT-14
	// recency-guard bookkeeping (§16 A33: event.time <= tenants.
	// last_event_at → projection unchanged, still recorded in
	// processed_events) and the lag between event.time and consumer
	// apply time. Any IAM service consuming Realm-Provisioner-produced
	// tenant-lifecycle events with the same last-writer-wins recency
	// guard (Delegation, Tender-ACL, and Group-Mapping services all
	// subscribe to overlapping lifecycle events per the ADR-0008
	// cascade pattern) can emit these with identical semantics, so both
	// stay domain-shared. LifecycleEventLagSeconds's sustained-high-P99
	// is the SLO-3 primary drift signal.
	LifecycleEventSkipped    *prometheus.CounterVec
	LifecycleEventLagSeconds *prometheus.HistogramVec

	// ── Tier 3 — iam_org_membership_* (unique to this service) ──────────

	// UnknownEventAcknowledged counts events consumed off tenant-orgm-q /
	// billing-orgm-q with a type this service does not handle (§6, event
	// consumer scope — silently ack + log + metric for forward-compat).
	// A sustained nonzero rate pages: add a handler for the surfacing
	// type. This service's own handled-type set is what makes an event
	// "unknown", so it can't be domain-shared.
	UnknownEventAcknowledged *prometheus.CounterVec

	// DelegateSuspendImpact counts P-7 suspensions where the user was a
	// delegate on active workflows and the WFI-13 advisory `delegate_impact`
	// warning fired (§16 C3, §8.8.5). Distinct from delegate_removal_blocked
	// — this is a non-fatal advisory, never a 409.
	DelegateSuspendImpact *prometheus.CounterVec

	// TenantOwnerlessEscalated counts the moment a removal drops the last
	// active tenant_owner. Event-time signal (distinct from the periodic
	// TenantOwnerless gauge, which is a scan). LLD §11.4 / TM-12 line 3807.
	TenantOwnerlessEscalated *prometheus.CounterVec

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

	// MembershipExistsCheck counts I-15 grant-time membership-existence
	// checks served, by caller and result (§11.2). Only this service owns
	// membership data, so only it can serve this check — not domain-shared.
	MembershipExistsCheck *prometheus.CounterVec

	// Business-observability gauges populated by 5-min exporter goroutines
	// in main.go (§11.2).
	TenantOwnerless         prometheus.Gauge // T-13
	RealmSyncPending        prometheus.Gauge // T-15
	SeatOverageActive       prometheus.Gauge // SEAT-5
	PendingInvitationsStale prometheus.Gauge // invitation-expiry cron health
)

// IncMessagesReceived/Processed/Failed record one SQS message's outcome
// against platform_messages_{received,processed,failed}_total, labelled by
// queue. Nil-safe, see ObserveDependencyLatency.
func IncMessagesReceived(queue string) {
	if MessagesReceived != nil {
		MessagesReceived.WithLabelValues(queue).Inc()
	}
}

func IncMessagesProcessed(queue string) {
	if MessagesProcessed != nil {
		MessagesProcessed.WithLabelValues(queue).Inc()
	}
}

func IncMessagesFailed(queue string) {
	if MessagesFailed != nil {
		MessagesFailed.WithLabelValues(queue).Inc()
	}
}

// ObserveDependencyLatency records a cross-service call's duration against
// the downstream peer (targetService — "catalog"|"group_mapping"|
// "delegation"), not this service's own identity (that's the metric's
// "service" const label, stamped centrally below). Nil-safe —
// DependencyRequestSeconds is only non-nil once Register() has run (server
// startup), so client/service unit tests that never call Register() get a
// silent no-op rather than a nil-pointer panic.
func ObserveDependencyLatency(targetService, endpoint string, seconds float64) {
	if DependencyRequestSeconds != nil {
		DependencyRequestSeconds.WithLabelValues(targetService, endpoint).Observe(seconds)
	}
}

// IncDependencyError records a cross-service call failure by outcome,
// against the downstream peer (targetService). Nil-safe, see
// ObserveDependencyLatency.
func IncDependencyError(targetService, endpoint, outcome string) {
	if DependencyErrors != nil {
		DependencyErrors.WithLabelValues(targetService, endpoint, outcome).Inc()
	}
}

// IncMembershipExistsCheck records an I-15 grant-time membership-existence
// check. Nil-safe, see ObserveDependencyLatency.
func IncMembershipExistsCheck(caller, result string) {
	if MembershipExistsCheck != nil {
		MembershipExistsCheck.WithLabelValues(caller, result).Inc()
	}
}

// IncRealmSyncFailed increments
// iam_org_membership_realm_sync_failed_total{stage=...}. Nil-safe, see
// ObserveDependencyLatency — safe to call before Register() runs.
func IncRealmSyncFailed(stage string) {
	if RealmSyncFailed != nil {
		RealmSyncFailed.WithLabelValues(stage).Inc()
	}
}

// Recorder is a zero-size handle satisfying cmd/reconciler/jobs.Metrics —
// its methods delegate to this package's existing nil-safe package-level
// counters (same convention as ObserveDependencyLatency/IncDependencyError),
// so wiring a Recorder into jobs.Context costs nothing beyond the interface
// satisfaction Context needs. Mirrors iam-delegation's jobs.Context.Metrics
// seam, adapted to this package's free-function-over-package-vars style
// rather than delegation's stateful *metrics.Metrics struct.
type Recorder struct{}

// IncRealmSyncFailed satisfies jobs.Metrics.
func (Recorder) IncRealmSyncFailed(stage string) { IncRealmSyncFailed(stage) }

// DependencyOutcome classifies a cross-service client transport error into
// one of platform_dependency_errors_total's two client-observable outcomes
// ("timeout" | "5xx"). The third outcome, "fallback_served", is recorded by
// the calling service layer, not here — only it knows whether a stale/
// last-known-good value was served instead of surfacing the error.
func DependencyOutcome(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "5xx"
}

// platformLabels returns the ConstLabels for a Tier-1 platform_* collector:
// domain (fixed "iam"), environment (Register's caller-supplied argument),
// and gincommon's {service, version} — centrally injected here (rule 8) so
// no Inc()/Observe() call site can omit or misspell "domain"/"service"/
// "environment".
func platformLabels(environment string) prometheus.Labels {
	out := prometheus.Labels{"domain": domain, "environment": environment}
	maps.Copy(out, gincommon.MetricsConstLabels())
	return out
}

// serviceLabels returns the ConstLabels for a Tier-2 (iam_*) or Tier-3
// (iam_org_membership_*) collector: environment plus gincommon's
// {service, version} — no "domain" label, since the iam_ namespace itself
// already encodes domain per the standard's Tier-2 required-labels list.
func serviceLabels(environment string) prometheus.Labels {
	out := prometheus.Labels{"environment": environment}
	maps.Copy(out, gincommon.MetricsConstLabels())
	return out
}

var registerOnce sync.Once

// Register wires business metrics onto gincommon's Prometheus registerer
// (same registry as HTTP metrics), with environment stamped as a const
// label on every collector. Call once at startup AFTER
// ObservabilityMiddlewares has run and BEFORE the /metrics endpoint is
// served. Idempotent — only the first call's environment takes effect.
func Register(environment string) {
	registerOnce.Do(func() { registerMetrics(environment) })
}

func registerMetrics(environment string) {
	pLabels := platformLabels(environment)
	sLabels := serviceLabels(environment)

	// ── Tier 1 — platform_* ──────────────────────────────────────────────
	MessagesReceived = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_messages_received_total",
		Help:        "Inbound queue messages dequeued, by queue, before processing.",
		ConstLabels: pLabels,
	}, []string{"queue"})

	MessagesProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_messages_processed_total",
		Help:        "Inbound queue messages that completed processing successfully, by queue.",
		ConstLabels: pLabels,
	}, []string{"queue"})

	MessagesFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_messages_failed_total",
		Help:        "Inbound queue messages whose processing returned an error, by queue (includes DLQ rejections).",
		ConstLabels: pLabels,
	}, []string{"queue"})

	DuplicateMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_duplicate_messages_total",
		Help:        "Redeliveries filtered by the processed_events composite PK (IDEMP-4), by consumer.",
		ConstLabels: pLabels,
	}, []string{"consumer"})

	DLQMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_dlq_messages_total",
		Help:        "Events actively rejected to DLQ without recording processed_events, by event_type and reason. Any nonzero rate pages.",
		ConstLabels: pLabels,
	}, []string{"event_type", "reason"})

	DependencyRequestSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "platform_dependency_request_seconds",
		Help:        "Latency of synchronous cross-service dependency calls, by target_service and endpoint.",
		Buckets:     []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 3},
		ConstLabels: pLabels,
	}, []string{"target_service", "endpoint"})

	DependencyErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "platform_dependency_errors_total",
		Help:        "Cross-service dependency call failures by target_service/endpoint/outcome (5xx|timeout|fallback_served).",
		ConstLabels: pLabels,
	}, []string{"target_service", "endpoint", "outcome"})

	// ── Tier 2 — iam_* ───────────────────────────────────────────────────
	RLSViolations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_rls_violations_total",
		Help:        "Row-level-security violations scraped from rls_violation_log, by violation_type.",
		ConstLabels: sLabels,
	}, []string{"violation_type"})

	AuthSessionRevokeFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_auth_session_revoke_failed_total",
		Help:        "RP RevokeUserSessions calls that returned non-2xx or errored (AUTH-8 fail-open).",
		ConstLabels: sLabels,
	}, []string{"reason"})

	LifecycleEventSkipped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_lifecycle_event_skipped_total",
		Help:        "Lifecycle events skipped by the EVT-14 recency guard (event.time <= tenants.last_event_at).",
		ConstLabels: sLabels,
	}, []string{"event_type"})

	LifecycleEventLagSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "iam_lifecycle_event_lag_seconds",
		Help:        "Seconds between event.time and consumer apply time. Sustained high P99 flags backlog (SLO-3).",
		Buckets:     []float64{0.05, 0.1, 0.5, 1, 5, 15, 60, 300, 1800},
		ConstLabels: sLabels,
	}, []string{"event_type"})

	// ── Tier 3 — iam_org_membership_* ───────────────────────────────────
	UnknownEventAcknowledged = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_org_membership_unknown_event_acknowledged_total",
		Help:        "Events silently acknowledged because no handler is wired for the type — sustained nonzero rate means a producer added a new type.",
		ConstLabels: sLabels,
	}, []string{"topic", "event_type"})

	DelegateSuspendImpact = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_org_membership_delegate_suspend_impact_total",
		Help:        "P-7 suspensions where the user was a delegate on active workflows and the WFI-13 advisory fired (advisory, never a block).",
		ConstLabels: sLabels,
	}, []string{"checked"})

	TenantOwnerlessEscalated = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_org_membership_tenant_ownerless_escalated_total",
		Help:        "Count of tenants that just entered the ownerless state on this write (TM-12). Every increment should page.",
		ConstLabels: sLabels,
	}, []string{"reason"})

	SeatOverageStarted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_org_membership_seat_overage_started_total",
		Help:        "Transitions from under-cap to over-cap on tenant seat consumption (SEAT-5).",
		ConstLabels: sLabels,
	}, []string{"cause"})

	SeatLimitReached = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_org_membership_seat_limit_reached_total",
		Help:        "P-6 invite attempts blocked by SEAT-1 cap.",
		ConstLabels: sLabels,
	}, []string{"plan"})

	InviteThrottled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_org_membership_invite_throttled_total",
		Help:        "P-6 invites rate-limited (§16 A41).",
		ConstLabels: sLabels,
	}, []string{"reason"})

	RealmSyncFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_org_membership_realm_sync_failed_total",
		Help:        "realm-config-sync reconciler failures (T-15).",
		ConstLabels: sLabels,
	}, []string{"stage"})

	DelegateRemovalBlocked = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_org_membership_delegate_removal_blocked_total",
		Help:        "P-7 removals blocked by WFI-3 delegate-impact pre-check (409 workflow_resolution_required).",
		ConstLabels: sLabels,
	}, []string{"scope"})

	DelegateReassignment = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_org_membership_delegate_reassignment_total",
		Help:        "P-26 removal-resolution completions by action (replace_delegate | stop_workflows).",
		ConstLabels: sLabels,
	}, []string{"action"})

	MembershipExistsCheck = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_org_membership_membership_exists_check_total",
		Help:        "I-15 grant-time membership-existence checks served, by caller and result.",
		ConstLabels: sLabels,
	}, []string{"caller", "result"})

	TenantOwnerless = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_org_membership_tenant_ownerless",
		Help:        "Tenants with ownerless_since IS NOT NULL (T-13). Sustained >0 pages.",
		ConstLabels: sLabels,
	})
	RealmSyncPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_org_membership_realm_sync_pending",
		Help:        "Tenants with realm_sync_pending=true (T-15).",
		ConstLabels: sLabels,
	})
	SeatOverageActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_org_membership_seat_overage_active",
		Help:        "Tenants with overage_since IS NOT NULL (SEAT-5).",
		ConstLabels: sLabels,
	})
	PendingInvitationsStale = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "iam_org_membership_pending_invitations_stale",
		Help:        "Pending invitations past expires_at that invitation-expiry hasn't flipped yet.",
		ConstLabels: sLabels,
	})

	gincommon.MetricsRegisterer().MustRegister(
		MessagesReceived,
		MessagesProcessed,
		MessagesFailed,
		DuplicateMessages,
		DLQMessages,
		DependencyRequestSeconds,
		DependencyErrors,
		RLSViolations,
		AuthSessionRevokeFailed,
		LifecycleEventSkipped,
		LifecycleEventLagSeconds,
		UnknownEventAcknowledged,
		DelegateSuspendImpact,
		TenantOwnerlessEscalated,
		SeatOverageStarted,
		SeatLimitReached,
		InviteThrottled,
		RealmSyncFailed,
		DelegateRemovalBlocked,
		DelegateReassignment,
		MembershipExistsCheck,
		TenantOwnerless,
		RealmSyncPending,
		SeatOverageActive,
		PendingInvitationsStale,
	)

	// Pre-initialise labels so dashboards show 0 rather than "no data".
	RLSViolations.WithLabelValues("missing_or_invalid_guc")
	RLSViolations.WithLabelValues("cross_tenant_access")
	AuthSessionRevokeFailed.WithLabelValues("transport")
	RealmSyncFailed.WithLabelValues("patch_realm_config")
	RealmSyncFailed.WithLabelValues("clear_marker")
}
