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
// ConstLabels by gincommon's constructors (NewShared*Vec for Tiers 1-2,
// NewCounterVec / NewGauge / NewHistogramVec for Tier 3), so instrumentation callers cannot
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
	"slices"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

var (
	// ── Tier 1 — platform_* (shared across domains) ─────────────────────

	// DuplicateMessages (platform_duplicate_messages_total{queue,
	// event_type}, Proposed) counts SQS redeliveries filtered by the
	// processed_events composite PK (IDEMP-4 / PE-1). This service dedups
	// with its own ledger, not platform-events' inbox, so only it can
	// record them. queue comes from the context set by WithQueue.
	DuplicateMessages *prometheus.CounterVec

	// DLQMessages (platform_dlq_messages_total{operation,event_type,
	// reason}, Approved) is shared with platform-events, which counts every
	// dead-letter exactly once: its own forwards (malformed, decode_error,
	// max_receive_count), this service's explicit rejects forwarded through
	// its DLQPublisher (reason="explicit", sqspipeline/dlq.go), and outbox
	// dead-letters (operation="outbox_publish"). This service never
	// increments it itself; why a message was rejected is
	// ConsumedEventRejected below.
	DLQMessages *prometheus.CounterVec

	// ConsumedEventRejected (iam_org_membership_consumed_event_rejected_total
	// {event_type,reason}) counts consumed events this service rejects as
	// permanent failures, at the reject site:
	//   - future_time_clamp — EVT-15 (§16 A40: event.time > now() + skew,
	//     a poison-pill/producer-clock-skew guard);
	//   - schema_violation — a consumed payload failing its embedded JSON
	//     Schema (sqspipeline/inbound_schema.go), i.e. a producer contract
	//     break.
	// Each is then forwarded to the DLQ. Any nonzero rate of either pages.
	ConsumedEventRejected *prometheus.CounterVec

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
	// failures by reason (timeout|transport|http_4xx|http_5xx|http_other|
	// fallback_served; "fallback_served"
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
	// returned non-2xx or errored (AUTH-8 fail-open). reason is one of the
	// registry's values: transport, http_4xx, http_5xx, http_other. AUTH-* is a shared
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

	// CatalogCacheInvalidated counts DepartmentCatalogChanged and
	// PlanCatalogChanged events that successfully cleared their respective
	// catalog cache keys (om:departments or om:plans, primary + stale) via
	// the catalog-orgm-q consumer (Gap-12 / CAT-GAP-004 fix). Provides a
	// Prometheus signal for the cache-propagation path.
	CatalogCacheInvalidated prometheus.Counter

	// AuditDeliveryFailed counts best-effort AL-5 direct-write calls that
	// returned an error from the fire-and-forget goroutines in TenantService
	// and InvitationService. Audit outages do not fail P-2 or invite flows
	// (fail-open by design), but a sustained nonzero rate means audit records
	// are silently missing and should alert.
	AuditDeliveryFailed *prometheus.CounterVec

	// PlanChangeCheckTotal counts I-18 plan-change pre-check outcomes
	// (CD-20) by result: allowed, blocked, or error (RP unavailable).
	PlanChangeCheckTotal *prometheus.CounterVec

	// Business-observability gauges populated by 5-min exporter goroutines
	// in main.go (§11.2).
	TenantOwnerless         prometheus.Gauge // T-13
	RealmSyncPending        prometheus.Gauge // T-15
	SeatOverageActive       prometheus.Gauge // SEAT-5
	PendingInvitationsStale prometheus.Gauge // invitation-expiry cron health
)

// IncDuplicate records one redelivery filtered by processed_events on the
// authoritative Tier-3 iam_org_membership_processed_events_duplicates_
// total{consumer}. The Tier-1 platform_duplicate_messages_total{queue,
// event_type} is recorded by platform-events' inbox on the same duplicate.
// Nil-safe.
func IncDuplicate(_ context.Context, consumer, _ string) {
	if ProcessedEventsDuplicatesTotal != nil {
		ProcessedEventsDuplicatesTotal.WithLabelValues(consumer).Inc()
	}
}

// IncConsumedEventRejected records one consumed event this service rejected
// as a permanent failure (reason future_time_clamp or schema_violation).
// The DLQ forward itself is counted by platform-events. Nil-safe.
func IncConsumedEventRejected(eventType, reason string) {
	if ConsumedEventRejected != nil {
		ConsumedEventRejected.WithLabelValues(eventLabel(eventType), reason).Inc()
	}
}

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
		DependencyCallDurationSeconds.WithLabelValues(targetService, endpointLabel(endpoint)).Observe(seconds)
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
	DependencyRequestSeconds.WithLabelValues(targetService, endpointLabel(endpoint), outcome).Observe(seconds)
}

// endpointLabel spells an endpoint label value in snake_case (callers name
// some endpoints after their URL path, e.g. "group-resolution"), so every
// dependency series uses one spelling.
func endpointLabel(endpoint string) string { return strings.ReplaceAll(endpoint, "-", "_") }

// IncDependencyError records a cross-service call failure by reason
// (see DependencyErrorReason / HTTPStatusReason / DependencyReasonFallbackServed) on the authoritative
// iam_org_membership_dependency_call_failures_total. Nil-safe.
func IncDependencyError(targetService, endpoint, reason string) {
	if DependencyCallFailuresTotal != nil {
		DependencyCallFailuresTotal.WithLabelValues(targetService, endpointLabel(endpoint), reason).Inc()
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

// Label values for the business-decision counters below. Fixed sets, so the
// series are bounded and pre-warmed.
const (
	DelegateBlockScopeTenantRemoval = "tenant_removal"
	DelegateBlockScopeDeptRemoval   = "dept_removal"
	DelegateBlockScopeDeptDemotion  = "dept_demotion"

	InviteThrottleReinviteTooSoon = "reinvite_too_soon"
	InviteThrottleTenantRate      = "tenant_rate_limited"
	InviteThrottleActorRate       = "actor_rate_limited"

	SeatOverageCauseReconciler = "occupancy"
)

// IncDelegateRemovalBlocked counts a removal or demotion refused with 409
// workflow_resolution_required. Nil-safe.
func IncDelegateRemovalBlocked(scope string) {
	if DelegateRemovalBlocked != nil {
		DelegateRemovalBlocked.WithLabelValues(scope).Inc()
	}
}

// IncDelegateReassignment counts a completed P-26 removal resolution by
// action (replace_delegate | stop_workflows). Nil-safe.
func IncDelegateReassignment(action string) {
	if DelegateReassignment != nil {
		DelegateReassignment.WithLabelValues(action).Inc()
	}
}

// IncInviteThrottled counts a P-6 invite refused by an abuse limit. Nil-safe.
func IncInviteThrottled(reason string) {
	if InviteThrottled != nil {
		InviteThrottled.WithLabelValues(reason).Inc()
	}
}

// IncSeatLimitReached counts a P-6 invite refused at the SEAT-1 cap, by the
// tenant's plan. Nil-safe.
func IncSeatLimitReached(plan string) {
	if SeatLimitReached != nil {
		SeatLimitReached.WithLabelValues(plan).Inc()
	}
}

// IncSeatOverageStarted counts a tenant entering seat overage. Nil-safe.
func IncSeatOverageStarted(cause string) {
	if SeatOverageStarted != nil {
		SeatOverageStarted.WithLabelValues(cause).Inc()
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

// IncSeatOverageStarted satisfies jobs.Metrics.
func (Recorder) IncSeatOverageStarted() { IncSeatOverageStarted(SeatOverageCauseReconciler) }

// IncCatalogCacheInvalidated records one successful om:departments cache
// clear by the catalog-orgm-q consumer. Nil-safe.
func IncCatalogCacheInvalidated() {
	if CatalogCacheInvalidated != nil {
		CatalogCacheInvalidated.Inc()
	}
}

// IncAuditDeliveryFailed records one AL-5 direct-write failure from a
// fire-and-forget audit goroutine. entry_type identifies which entry type
// failed (e.g. "config.tenant_setting.changed", "invitation.created").
// Nil-safe.
func IncAuditDeliveryFailed(entryType string) {
	if AuditDeliveryFailed != nil {
		AuditDeliveryFailed.WithLabelValues(entryType).Inc()
	}
}

// IncPlanChangeCheck records one I-18 plan-change pre-check outcome.
// result must be one of "allowed", "blocked", "error". Nil-safe.
func IncPlanChangeCheck(result string) {
	if PlanChangeCheckTotal != nil {
		PlanChangeCheckTotal.WithLabelValues(result).Inc()
	}
}

// ObserveLifecycleEventLag records the seconds between event.time and the
// moment this consumer applied the projection — the SLO-3 primary drift
// signal. Nil-safe, see ObserveDependencyLatency.
func ObserveLifecycleEventLag(eventType string, seconds float64) {
	if LifecycleEventLagSeconds != nil {
		LifecycleEventLagSeconds.WithLabelValues(eventType).Observe(seconds)
	}
}

// Dependency failure reasons for iam_org_membership_dependency_call_failures_total.
// The registry's reason vocabulary must start with a letter, so status
// classes are spelled http_4xx/http_5xx rather than "5xx".
const (
	DependencyReasonTimeout        = "timeout"
	DependencyReasonTransport      = "transport"
	DependencyReasonFallbackServed = "fallback_served"
)

// DependencyErrorReason classifies a cross-service client transport error as
// "timeout" or "transport". "fallback_served" is recorded by the calling
// service layer, not here — only it knows whether a stale/last-known-good
// value was served instead of surfacing the error.
func DependencyErrorReason(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return DependencyReasonTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return DependencyReasonTimeout
	}
	return DependencyReasonTransport
}

// HTTPStatusReason maps a non-2xx response status onto the registry-safe
// reason values http_4xx | http_5xx | http_other.
func HTTPStatusReason(status int) string {
	switch {
	case status >= 400 && status < 500:
		return "http_4xx"
	case status >= 500 && status < 600:
		return "http_5xx"
	default:
		return "http_other"
	}
}

var (
	registerMu   sync.Mutex
	registerDone bool
)

// Register builds and registers every collector this service owns onto
// gincommon's registerer (the same registry as the HTTP metrics). Call it
// after ObservabilityMiddlewares (which fixes the {domain, service,
// environment} identity), before events.InitMetrics (so platform-events
// adopts the registry-shared collectors instead of registering its own)
// and before /metrics is served.
//
// Every constructor is gincommon's: Tiers 1 and 2 from the Platform
// Observability Registry (NewShared*Vec), Tier 3 through NewCounterVec /
// NewGauge / NewHistogramVec, which validate the iam_org_membership_ prefix,
// suffixes and prohibited labels and inject the identity const labels. They
// need that identity, so until gincommon is initialised Register is a
// no-op and the nil-safe helpers record nothing; the first call after
// initialisation builds everything. Idempotent. environment is kept for
// callers' sake; the identity comes from gincommon.
func Register(_ string) {
	registerMu.Lock()
	defer registerMu.Unlock()
	if registerDone || len(gincommon.MetricsConstLabels()) == 0 {
		return
	}
	registerShared()
	registerServiceMetrics()
	prewarm()
	registerDone = true
}

// registerShared builds the registry-owned Tier-1 and Tier-2 collectors. A
// collector platform-events (or an earlier registration) already holds is
// adopted rather than duplicated, so the call is order-tolerant; any other
// registration error panics, like MustRegister.
func registerShared() {
	DuplicateMessages = mustShared(gincommon.NewSharedCounterVec(nil, "platform_duplicate_messages_total"))
	DLQMessages = mustShared(gincommon.NewSharedCounterVec(nil, "platform_dlq_messages_total"))
	DependencyRequestSeconds = mustShared(gincommon.NewSharedHistogramVec(nil, "platform_dependency_request_seconds"))

	// Tier 2 — domain-shared iam_* entries (platform-gincommon v1.6.0):
	// name, help, labels and buckets come from the registry, so every IAM
	// service emitting them stays identical.
	RLSViolations = mustShared(gincommon.NewSharedCounterVec(nil, "iam_rls_violations_total"))
	AuthSessionRevokeFailed = mustShared(gincommon.NewSharedCounterVec(nil, "iam_auth_session_revoke_failed_total"))
	LifecycleEventSkipped = mustShared(gincommon.NewSharedCounterVec(nil, "iam_lifecycle_event_skipped_total"))
	LifecycleEventLagSeconds = mustShared(gincommon.NewSharedHistogramVec(nil, "iam_lifecycle_event_lag_seconds"))
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

// must panics on a Tier 3 construction error: a name, suffix or label the
// standard rejects is a programming error caught at startup.
func must[C any](c C, err error) C {
	if err != nil {
		panic("metrics: register service collector: " + err.Error())
	}
	return c
}

// rlsViolationTypes is the registry's fixed violation_type set for
// iam_rls_violations_total.
var rlsViolationTypes = []string{"cross_tenant_access", "missing_or_invalid_guc", "missing_primary_email", "other"}

// AddRLSViolations adds n to iam_rls_violations_total{violation_type}.
// A violation_type outside the registry's set is recorded as "other".
// Nil-safe.
func AddRLSViolations(violationType string, n float64) {
	if RLSViolations == nil {
		return
	}
	if !slices.Contains(rlsViolationTypes, violationType) {
		violationType = "other"
	}
	RLSViolations.WithLabelValues(violationType).Add(n)
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

// registerServiceMetrics builds the Tier 3 iam_org_membership_* collectors.
func registerServiceMetrics() {
	// Tier 3 predecessors of the Proposed Tier 1 candidates: authoritative
	// until ratification (PlatformRegistry, registry.go).
	DependencyCallDurationSeconds = must(gincommon.NewHistogramVec(nil,
		"iam_org_membership_dependency_call_duration_seconds",
		"Latency of synchronous cross-service dependency calls, by target_service and endpoint. Authoritative alerting/SLO source until platform_dependency_request_seconds (Proposed, registry.go) is ratified.",
		[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 3},
		"target_service", "endpoint"))
	DependencyCallFailuresTotal = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_dependency_call_failures_total",
		"Cross-service dependency call failures by target_service/endpoint/reason (timeout|transport|http_4xx|http_5xx|http_other|fallback_served). Authoritative failure counter; platform_dependency_request_seconds_count{outcome=\"error\"} is the registry concept.",
		"target_service", "endpoint", "reason"))
	ProcessedEventsDuplicatesTotal = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_processed_events_duplicates_total",
		"SQS redeliveries filtered by the processed_events composite PK (IDEMP-4). Authoritative alerting source until platform_duplicate_messages_total (Proposed, registry.go) is ratified.",
		"consumer"))

	UnknownEventAcknowledged = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_unknown_event_acknowledged_total",
		"Events silently acknowledged because no handler is wired for the type — sustained nonzero rate means a producer added a new type.",
		"event_type"))
	DelegateSuspendImpact = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_delegate_suspend_impact_total",
		"P-7 suspensions where the user was a delegate on active workflows and the WFI-13 advisory fired (advisory, never a block).",
		"checked"))
	TenantOwnerlessEscalated = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_tenant_ownerless_escalated_total",
		"Count of tenants that just entered the ownerless state on this write (TM-12). Every increment should page.",
		"reason"))
	SeatOverageStarted = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_seat_overage_started_total",
		"Tenants entering seat overage (SEAT-5), by cause: occupancy (active members plus pending invitations above licensed_seats, detected by the seat-overage reconciler).",
		"cause"))
	SeatLimitReached = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_seat_limit_reached_total",
		"P-6 invites refused because active members plus pending invitations reached licensed_seats (SEAT-1), by tenant plan.",
		"plan"))
	InviteThrottled = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_invite_throttled_total",
		"P-6 invites refused by the abuse limits (§16 A41), by reason: reinvite_too_soon (PI-11), tenant_rate_limited and actor_rate_limited (PI-12).",
		"reason"))
	RealmSyncFailed = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_realm_sync_failed_total",
		"realm-config-sync reconciler failures (T-15).",
		"stage"))
	DelegateRemovalBlocked = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_delegate_removal_blocked_total",
		"Removals or demotions refused because the user is a delegate on active workflows (409 workflow_resolution_required), by scope: tenant_removal (P-8 WFI-3), dept_removal (P-11), dept_demotion (P-10 level decrease, WFI-9/12).",
		"scope"))
	DelegateReassignment = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_delegate_reassignment_total",
		"P-26 removal-resolution completions by action (replace_delegate | stop_workflows).",
		"action"))
	MembershipExistsCheck = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_membership_exists_check_total",
		"I-15 grant-time membership-existence checks served, by caller and result.",
		"caller", "result"))
	ConsumedEventRejected = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_consumed_event_rejected_total",
		"Consumed events rejected as permanent failures (EVT-15 future-time clamp, consumed-schema violation) and forwarded to the DLQ, by event type and reason.",
		"event_type", "reason"))
	PublicTenantResolve = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_public_tenant_resolve_total",
		"PUB-1 pre-login tenant/realm resolution lookups served, by result.",
		"result"))
	CatalogCacheInvalidated = must(gincommon.NewCounter(nil,
		"iam_org_membership_catalog_cache_invalidated_total",
		"Catalog cache invalidations via catalog-orgm-q: DepartmentCatalogChanged (om:departments) and PlanCatalogChanged (om:plans) events that successfully cleared their primary+stale cache keys (Gap-12/CAT-GAP-004)."))
	AuditDeliveryFailed = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_audit_delivery_failed_total",
		"AL-5 direct-write calls that returned an error from best-effort fire-and-forget audit goroutines (TenantService, InvitationService). Audit outages are fail-open; a sustained nonzero rate means audit records are silently missing.",
		"entry_type"))
	PlanChangeCheckTotal = must(gincommon.NewCounterVec(nil,
		"iam_org_membership_plan_change_check_total",
		"I-18 plan-change pre-check (CD-20) outcomes by result: allowed (no SSO blocker), blocked (sso_idp_configured), error (RP unavailable — maps to 503).",
		"result"))

	TenantOwnerless = must(gincommon.NewGauge(nil,
		"iam_org_membership_tenant_ownerless",
		"Tenants with ownerless_since IS NOT NULL (T-13). Sustained >0 pages."))
	RealmSyncPending = must(gincommon.NewGauge(nil,
		"iam_org_membership_realm_sync_pending",
		"Tenants with realm_sync_pending=true (T-15)."))
	SeatOverageActive = must(gincommon.NewGauge(nil,
		"iam_org_membership_seat_overage_active",
		"Tenants with overage_since IS NOT NULL (SEAT-5)."))
	PendingInvitationsStale = must(gincommon.NewGauge(nil,
		"iam_org_membership_pending_invitations_stale",
		"Pending invitations past expires_at that invitation-expiry hasn't flipped yet."))
}

// prewarm initialises bounded label combinations so counters appear at 0
// on startup rather than only after the first event (OBS-8).
func prewarm() {
	for _, vt := range rlsViolationTypes {
		RLSViolations.WithLabelValues(vt)
	}
	for _, reason := range []string{"transport", "http_4xx", "http_5xx", "http_other"} {
		AuthSessionRevokeFailed.WithLabelValues(reason)
	}
	for _, et := range handledEventTypes {
		if et != "DepartmentCatalogChanged" { // not a lifecycle event: no recency guard
			LifecycleEventSkipped.WithLabelValues(et)
		}
		ConsumedEventRejected.WithLabelValues(et, "future_time_clamp")
		ConsumedEventRejected.WithLabelValues(et, "schema_violation")
	}
	RealmSyncFailed.WithLabelValues("patch_realm_config")
	RealmSyncFailed.WithLabelValues("clear_marker")
	for _, scope := range []string{DelegateBlockScopeTenantRemoval, DelegateBlockScopeDeptRemoval, DelegateBlockScopeDeptDemotion} {
		DelegateRemovalBlocked.WithLabelValues(scope)
	}
	for _, action := range []string{"replace_delegate", "stop_workflows"} {
		DelegateReassignment.WithLabelValues(action)
	}
	for _, reason := range []string{InviteThrottleReinviteTooSoon, InviteThrottleTenantRate, InviteThrottleActorRate} {
		InviteThrottled.WithLabelValues(reason)
	}
	SeatOverageStarted.WithLabelValues(SeatOverageCauseReconciler)
	// I-15: result is "active" or "inactive"; caller is open-ended (any
	// downstream service), so only the known peers are pre-warmed.
	for _, caller := range []string{"tender-acl", "delegation"} {
		MembershipExistsCheck.WithLabelValues(caller, "active")
		MembershipExistsCheck.WithLabelValues(caller, "inactive")
	}
	PublicTenantResolve.WithLabelValues("found")
	PublicTenantResolve.WithLabelValues("not_found")
	for _, r := range []string{"allowed", "blocked", "error"} {
		PlanChangeCheckTotal.WithLabelValues(r)
	}
	for _, et := range []string{
		"config.tenant_setting.changed",
		"invitation.created", "invitation.revoked", "invitation.expired",
	} {
		AuditDeliveryFailed.WithLabelValues(et)
	}
}
