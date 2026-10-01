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
//     "domain", "service", "environment" are injected centrally.
//   - Tier 3 — iam_org_membership_* : behavior unique to this one
//     service (seat caps, tenant-ownerless escalation, invitation
//     lifecycle, delegate-impact advisories, ...). No other IAM service
//     has an equivalent concept, so the service identity is baked into
//     the name itself rather than relied upon as a label.
//
// Every counter ends in _total; every histogram ends in _seconds. The
// "domain"/"service"/"environment" labels are never left to individual
// Inc()/Observe() call sites to supply — they're baked in once via
// ConstLabels (gincommon.MetricsConstLabels, see serviceLabels, and the
// registry-built Tier-1 collectors), so instrumentation callers cannot
// omit or misspell them (rule 8). No collector carries a version label;
// the build version lives only on platform_build_info.
//
// Phase 0 registered the minimal infra set; Phase 6 landed the full
// business metric surface per §11. This file was last reclassified to
// match the tiered platform_*/iam_*/iam_org_membership_* standard —
// see each var's doc comment for its tier and, for tier-1/2 promotions
// without a named example in the standard, the rationale.
//
// Tier-1 platform_* collectors are NOT hand-built here: they come from the
// central Platform Observability Registry via gincommon.NewShared*Vec, so
// their name, help, label set and buckets are exactly the registry's
// (platform-gincommon internal/obsregistry/registry.json). platform-events
// registers the same names on the same registerer; because the
// descriptors are identical it reuses these collectors, so the library
// (SQS dead-letters, outbox dead-letters, SNS/SQS dependency calls) and
// this service (its own DLQ rejects, duplicate filtering, peer HTTP calls)
// write ONE series set. Registration order is therefore load-bearing:
// Register must run after gincommon.ObservabilityMiddlewares and before
// events.InitMetrics (cmd/server/main.go, cmd/reconciler/main.go).
//
// The consumer-lifecycle counters platform_messages_{received,processed,
// failed}_total are recorded by platform-events' SQS consumer itself (per
// message, labelled queue/event_type/reason), so this service no longer
// records them.
//
// Two Tier-1 metrics this service writes — platform_dependency_request_
// seconds and platform_duplicate_messages_total — are still Proposed in the
// registry. Their Tier-3 counterparts (DependencyCallDurationSeconds,
// ProcessedEventsDuplicatesTotal, DependencyCallFailuresTotal) remain this
// repo's alerting/SLO source until ratification (registry.go,
// .github/scripts/metrics-registry-lint.sh). The former Tier-1
// dependency-errors shadow counter was removed: it was never in the
// registry; its registry concept is
// platform_dependency_request_seconds_count{outcome="error"}.
package metrics

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
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

	// DuplicateMessages (platform_duplicate_messages_total{queue,
	// event_type}, Proposed) counts SQS redeliveries filtered by the
	// processed_events composite PK (IDEMP-4 / PE-1). This service dedups
	// with its own ledger, not platform-events' inbox, so only it can
	// record them. queue comes from the context set by WithQueue.
	DuplicateMessages *prometheus.CounterVec

	// DLQMessages (platform_dlq_messages_total{operation,event_type,
	// reason}, Approved) is shared with platform-events: the library
	// records operation="consume" redrive dead-letters it performs and
	// operation="outbox_publish",reason="max_attempts" outbox dead-letters;
	// this service records the events it actively rejects to the DLQ
	// itself (cmd/server/dlq.go's raw-SQS router, which platform-events
	// never sees) as operation="consume" with reason:
	//   - future_time_clamp — EVT-15 (§16 A40: event.time > now() + skew,
	//     a poison-pill/producer-clock-skew guard);
	//   - schema_violation — a consumed payload failing its embedded JSON
	//     Schema (cmd/server/inbound_schema.go), i.e. a producer contract
	//     break.
	// Any nonzero rate of either pages; alerts filter on reason.
	DLQMessages *prometheus.CounterVec

	// DependencyRequestSeconds (platform_dependency_request_seconds{
	// dependency,operation,outcome}, Proposed) times the synchronous
	// cross-service client calls. dependency is the downstream peer
	// (catalog|group_mapping|delegation|realm_provisioner|workflow|
	// token_service), operation the client endpoint in snake_case,
	// outcome success|error (error = transport failure or 5xx). Shared
	// with platform-events, which records its sns/sqs/codec calls here.
	DependencyRequestSeconds *prometheus.HistogramVec

	// ── Tier 3 predecessors, shadow-authoritative until ratification ────

	// DependencyCallDurationSeconds is DependencyRequestSeconds's Tier-3
	// predecessor — this repo's actual alerting/SLO source for
	// cross-service dependency latency until platform_dependency_
	// request_seconds's PlatformRegistry entry (registry.go) flips from
	// Proposed to Canonical. Same call site, same values, different name.
	DependencyCallDurationSeconds *prometheus.HistogramVec

	// DependencyCallFailuresTotal counts cross-service dependency call
	// failures by outcome (5xx|timeout|fallback_served; "fallback_served"
	// is recorded by the calling service layer — CatalogService/
	// GroupMappingService — since only it knows whether a stale value was
	// served). It is the authoritative failure counter; the registry has
	// no platform_* equivalent with these outcomes (the closest is
	// platform_dependency_request_seconds_count{outcome="error"}).
	DependencyCallFailuresTotal *prometheus.CounterVec

	// ProcessedEventsDuplicatesTotal is DuplicateMessages's Tier-3
	// predecessor — authoritative until platform_duplicate_messages_total
	// ratifies (registry.go).
	ProcessedEventsDuplicatesTotal *prometheus.CounterVec

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

	// PublicTenantResolve counts PUB-1 pre-login tenant/realm lookups by
	// result, found|not_found (§11.2).
	PublicTenantResolve *prometheus.CounterVec

	// Business-observability gauges populated by 5-min exporter goroutines
	// in main.go (§11.2).
	TenantOwnerless         prometheus.Gauge // T-13
	RealmSyncPending        prometheus.Gauge // T-15
	SeatOverageActive       prometheus.Gauge // SEAT-5
	PendingInvitationsStale prometheus.Gauge // invitation-expiry cron health
)

type queueCtxKey struct{}

// WithQueue returns ctx carrying the logical queue name (e.g.
// tenant-orgm-q) a consumed message came from, so code deep in the
// consumer (dedup) can label platform_* series by queue without threading
// it through every handler signature. cmd/server's instrumentedHandler
// sets it per message.
func WithQueue(ctx context.Context, queue string) context.Context {
	return context.WithValue(ctx, queueCtxKey{}, queue)
}

// QueueFromContext returns the queue set by WithQueue, or "unknown" (the
// registry's value for an absent queue).
func QueueFromContext(ctx context.Context) string {
	if q, ok := ctx.Value(queueCtxKey{}).(string); ok && q != "" {
		return q
	}
	return "unknown"
}

// IncDuplicate records one redelivery filtered by processed_events against
// platform_duplicate_messages_total{queue,event_type} (queue from ctx) and
// the authoritative Tier-3 iam_org_membership_processed_events_duplicates_
// total{consumer}. Nil-safe.
func IncDuplicate(ctx context.Context, consumer, eventType string) {
	if ProcessedEventsDuplicatesTotal != nil {
		ProcessedEventsDuplicatesTotal.WithLabelValues(consumer).Inc()
	}
	if DuplicateMessages != nil {
		DuplicateMessages.WithLabelValues(QueueFromContext(ctx), eventLabel(eventType)).Inc()
	}
}

// IncDLQ records one event this service rejected to its DLQ, as
// platform_dlq_messages_total{operation="consume",event_type,reason}.
// Nil-safe.
func IncDLQ(eventType, reason string) {
	if DLQMessages != nil {
		DLQMessages.WithLabelValues(dlqOperationConsume, eventLabel(eventType), reason).Inc()
	}
}

// dlqOperationConsume is the registry operation value for a consumed
// message moved to dead-letter storage.
const dlqOperationConsume = "consume"

// eventLabel bounds an event_type label value the way the registry
// requires: "unknown" when empty, "__oversized__" past 128 bytes.
func eventLabel(eventType string) string {
	switch {
	case eventType == "":
		return "unknown"
	case len(eventType) > 128:
		return "__oversized__"
	default:
		return eventType
	}
}

// ObserveDependencyLatency records a cross-service call's duration on the
// authoritative Tier-3 iam_org_membership_dependency_call_duration_seconds
// {target_service,endpoint} only. HTTP clients use ObserveDependencyCall,
// which also records the registry's platform_dependency_request_seconds
// with an outcome. Nil-safe — collectors are only non-nil once Register()
// has run, so unit tests that never call Register() get a silent no-op.
func ObserveDependencyLatency(targetService, endpoint string, seconds float64) {
	if DependencyCallDurationSeconds != nil {
		DependencyCallDurationSeconds.WithLabelValues(targetService, endpoint).Observe(seconds)
	}
}

// ObserveDependencyCall records one synchronous call to a peer service:
// the Tier-3 latency histogram (see ObserveDependencyLatency) plus
// platform_dependency_request_seconds{dependency=targetService,
// operation=endpoint in snake_case, outcome}. outcome is "error" for a
// transport error (err != nil) or a 5xx response, else "success" — a 4xx is
// the caller's own request being rejected, not a dependency fault. resp may
// be nil when err is set. Nil-safe.
func ObserveDependencyCall(targetService, endpoint string, seconds float64, resp *http.Response, err error) {
	ObserveDependencyLatency(targetService, endpoint, seconds)
	if DependencyRequestSeconds == nil {
		return
	}
	outcome := "success"
	if err != nil || resp == nil || resp.StatusCode >= 500 {
		outcome = "error"
	}
	DependencyRequestSeconds.WithLabelValues(targetService, strings.ReplaceAll(endpoint, "-", "_"), outcome).Observe(seconds)
}

// IncDependencyError records a cross-service call failure by outcome
// (5xx|timeout|fallback_served) on the authoritative
// iam_org_membership_dependency_call_failures_total. Nil-safe.
func IncDependencyError(targetService, endpoint, outcome string) {
	if DependencyCallFailuresTotal != nil {
		DependencyCallFailuresTotal.WithLabelValues(targetService, endpoint, outcome).Inc()
	}
}

// IncMembershipExistsCheck records an I-15 grant-time membership-existence
// check. Nil-safe, see ObserveDependencyLatency.
func IncMembershipExistsCheck(caller, result string) {
	if MembershipExistsCheck != nil {
		MembershipExistsCheck.WithLabelValues(caller, result).Inc()
	}
}

// IncPublicTenantResolve records one PUB-1 lookup outcome ("found" or
// "not_found"). Nil-safe, see ObserveDependencyLatency.
func IncPublicTenantResolve(result string) {
	if PublicTenantResolve != nil {
		PublicTenantResolve.WithLabelValues(result).Inc()
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

// ObserveLifecycleEventLag records the seconds between event.time and the
// moment this consumer applied the projection — the SLO-3 primary drift
// signal. Nil-safe, see ObserveDependencyLatency.
func ObserveLifecycleEventLag(eventType string, seconds float64) {
	if LifecycleEventLagSeconds != nil {
		LifecycleEventLagSeconds.WithLabelValues(eventType).Observe(seconds)
	}
}

// DependencyOutcome classifies a cross-service client transport error into
// one of iam_org_membership_dependency_call_failures_total's two
// client-observable outcomes
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

// serviceLabels returns the ConstLabels for a Tier-2 (iam_*) or Tier-3
// (iam_org_membership_*) collector: gincommon's {domain, environment,
// service} (MetricsConstLabels) — the same identity every gincommon,
// platform-events and platform-pgcommon series carries, injected centrally
// (rule 8) so no Inc()/Observe() call site can omit or misspell it.
// environment (Register's argument) and domain="iam" are only a fallback
// for unit tests that register metrics without initialising gincommon.
func serviceLabels(environment string) prometheus.Labels {
	if labels := gincommon.MetricsConstLabels(); len(labels) > 0 {
		return labels
	}
	return prometheus.Labels{"domain": domain, "environment": environment}
}

var registerOnce sync.Once

var (
	sharedMu   sync.Mutex
	sharedDone bool
)

// Register wires business metrics onto gincommon's Prometheus registerer
// (same registry as HTTP metrics). Call once at startup AFTER
// ObservabilityMiddlewares has run, BEFORE events.InitMetrics (so
// platform-events reuses the shared Tier-1 collectors instead of
// registering its own) and BEFORE the /metrics endpoint is served.
// Idempotent. The Tier-1 collectors need gincommon's identity: if gincommon
// is not initialised yet (some unit tests), they are skipped and created
// by the first later Register call that finds it initialised.
func Register(environment string) {
	registerOnce.Do(func() { registerMetrics(environment) })
	registerShared()
}

// registerShared builds the registry-owned Tier-1 collectors once
// gincommon's identity is known. A collector platform-events (or an
// earlier registration) already holds is adopted rather than duplicated,
// so the call is order-tolerant; any other registration error panics, like
// MustRegister.
func registerShared() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedDone || len(gincommon.MetricsConstLabels()) == 0 {
		return
	}
	DuplicateMessages = mustShared(gincommon.NewSharedCounterVec(nil, "platform_duplicate_messages_total"))
	DLQMessages = mustShared(gincommon.NewSharedCounterVec(nil, "platform_dlq_messages_total"))
	DependencyRequestSeconds = mustShared(gincommon.NewSharedHistogramVec(nil, "platform_dependency_request_seconds"))
	sharedDone = true

	// DLQMessages — every handled event type crossed with both
	// service-side reject reasons, so dashboards show 0 rather than "no
	// data". event_type is the envelope type at the point of rejection
	// (EVT-15 fires before the handler runs, schema_violation fires for any
	// consumed type).
	for _, et := range handledEventTypes {
		DLQMessages.WithLabelValues(dlqOperationConsume, et, "future_time_clamp")
		DLQMessages.WithLabelValues(dlqOperationConsume, et, "schema_violation")
	}
}

// mustShared returns the collector gincommon built, or — when an identical
// collector is already registered (platform-events registered it first)
// — the existing one.
func mustShared[C prometheus.Collector](c C, err error) C {
	if err == nil {
		return c
	}
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		if existing, ok := are.ExistingCollector.(C); ok {
			return existing
		}
	}
	panic("metrics: register shared platform collector: " + err.Error())
}

// handledEventTypes is every event type this consumer handles (and so may
// reject to the DLQ), used to pre-warm label combinations.
var handledEventTypes = []string{
	"TrialTenantProvisioned", "TenantRealmReady", "TenantConverted",
	"DirectPaidSignup", "TrialExpired", "TrialReactivated",
	"TenantSuspended", "TenantOffboarded",
	"TenantPlanChanged", "TenantPaymentPastDue",
	"TenantSubscriptionCancelled", "TenantReactivated", "TenantSeatsChanged",
	"DepartmentCatalogChanged",
}

func registerMetrics(environment string) {
	sLabels := serviceLabels(environment)

	// Tier 1 — platform_* collectors are built by registerShared from the
	// central registry, not here.

	// ── Tier 3 predecessors, shadow-authoritative until ratification ────
	DependencyCallDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "iam_org_membership_dependency_call_duration_seconds",
		Help:        "Latency of synchronous cross-service dependency calls, by target_service and endpoint. Authoritative alerting/SLO source until platform_dependency_request_seconds (Proposed, registry.go) is ratified.",
		Buckets:     []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 3},
		ConstLabels: sLabels,
	}, []string{"target_service", "endpoint"})

	DependencyCallFailuresTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_org_membership_dependency_call_failures_total",
		Help:        "Cross-service dependency call failures by target_service/endpoint/outcome (5xx|timeout|fallback_served). Authoritative failure counter; platform_dependency_request_seconds_count{outcome=\"error\"} is the registry concept.",
		ConstLabels: sLabels,
	}, []string{"target_service", "endpoint", "outcome"})

	ProcessedEventsDuplicatesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_org_membership_processed_events_duplicates_total",
		Help:        "SQS redeliveries filtered by the processed_events composite PK (IDEMP-4). Authoritative alerting source until platform_duplicate_messages_total (Proposed, registry.go) is ratified.",
		ConstLabels: sLabels,
	}, []string{"consumer"})

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

	PublicTenantResolve = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "iam_org_membership_public_tenant_resolve_total",
		Help:        "PUB-1 pre-login tenant/realm resolution lookups served, by result.",
		ConstLabels: sLabels,
	}, []string{"result"})

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
		DependencyCallDurationSeconds,
		DependencyCallFailuresTotal,
		ProcessedEventsDuplicatesTotal,
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
		PublicTenantResolve,
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

	// OBS-8: pre-initialise bounded label combinations so counters appear at
	// 0 immediately on startup rather than only after the first event arrives.

	// LifecycleEventSkipped — EVT-14 recency guard fires for any applied
	// event type; pre-warm the same set of known handled types.
	for _, et := range []string{
		"TrialTenantProvisioned", "TenantRealmReady", "TenantConverted",
		"DirectPaidSignup", "TrialExpired", "TrialReactivated",
		"TenantSuspended", "TenantOffboarded",
		"TenantPlanChanged", "TenantPaymentPastDue",
		"TenantSubscriptionCancelled", "TenantReactivated", "TenantSeatsChanged",
	} {
		LifecycleEventSkipped.WithLabelValues(et)
	}

	// MembershipExistsCheck — I-15 result can only be "active" or "inactive".
	// caller is open-ended (any downstream service), so only the result axis
	// is pre-warmed; caller labels warm on first call from each peer.
	MembershipExistsCheck.WithLabelValues("tender-acl", "active")
	MembershipExistsCheck.WithLabelValues("tender-acl", "inactive")
	MembershipExistsCheck.WithLabelValues("delegation", "active")
	MembershipExistsCheck.WithLabelValues("delegation", "inactive")

	// PublicTenantResolve — PUB-1's result is bounded to two values.
	PublicTenantResolve.WithLabelValues("found")
	PublicTenantResolve.WithLabelValues("not_found")
}
