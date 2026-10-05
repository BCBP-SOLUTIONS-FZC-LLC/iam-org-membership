package metrics

// RegistryStatus is this service's local mirror of a metric's status in
// the Platform Observability Registry — the Enterprise Platform
// Observability Standard's governance body for every platform_* and
// domain-shared metric name and label vocabulary (rules 9-12: shared
// names/labels are part of the observability contract; a service SHALL
// NOT invent or adopt one unilaterally). There is no live registry
// service this repo can call, so PlatformRegistry below — including each
// entry's full ratification packet (semantic definition, label
// vocabulary and allowed values, cardinality justification, aggregation
// expectations) — is the checked-in source of truth this service's own
// CI enforces itself against (mirrors iam-realm-provisioner's own
// registry.go).
//
// Ratification checklist (for the reviewing body, once one exists):
//   - Semantic definition holds identically across at least two domains
//     (not just IAM) for the metric under review.
//   - The label vocabulary (RegistryEntry.RequiredLabels/ApprovedLabels/
//     LabelValues) is acceptable as canonical, or amended with a
//     documented reason.
//   - No approved label is high-cardinality (user_id/email/tenant_id/
//     request_id/event_id/session_id or an equivalent unbounded value) —
//     every entry below is reviewed against this before being added.
//   - Aggregation expectations are validated against at least one other
//     service's real usage before Canonical status is granted — a single
//     submitter's shape may not generalize.
//   - Once approved: flip the entry's Status to StatusCanonical, then
//     follow the standard's Backward Compatibility migration order
//     (dashboards → alerts → recording rules → SLOs → HPA → deprecate the
//     Tier 3 predecessor → remove after the approved sunset period).
type RegistryStatus string

const (
	// StatusCanonical marks a metric already named as a worked Tier 1/2
	// example in the standard itself, or one the registry has ratified.
	// Safe to adopt as an alerting/dashboard/recording-rule/SLO/HPA
	// source.
	StatusCanonical RegistryStatus = "canonical"

	// StatusProposed marks a metric submitted for ratification but not
	// yet approved. Per rule 12, a Proposed metric MUST be shadow-emitted
	// only — no alert, dashboard, recording rule, SLO, or HPA reference
	// in this repo may treat it as authoritative until its status here
	// flips to Canonical following real governance approval.
	StatusProposed RegistryStatus = "proposed"
)

// RegistryEntry documents one platform_*/domain-shared metric this service
// emits or proposes, carrying the exact information the standard's
// Registry Ratification Requirement calls for (semantic definition,
// required labels, allowed label values, aggregation expectations) so a
// reviewer — human or CI — can check actual instrumentation against it
// without re-deriving intent from the Go source.
type RegistryEntry struct {
	// Name is the exact metric name as registered with Prometheus.
	Name string

	// Tier is "platform" (Tier 1) or "domain" (Tier 2). Tier 3
	// (service-specific) metrics carry no registry entry — the standard
	// only governs shared names.
	Tier string

	Status RegistryStatus

	// SemanticDefinition is what this metric means, in domain-neutral
	// terms — must hold regardless of which service or domain emits it.
	SemanticDefinition string

	// RequiredLabels are the standard's mandated const labels for this
	// tier: {domain, service, environment} for Tier 1, {service,
	// environment} for Tier 2.
	RequiredLabels []string

	// ApprovedLabels are the additional dynamic labels this entry's
	// packet requests approval for, beyond RequiredLabels. A label not
	// listed here or in RequiredLabels is not part of this metric's
	// approved vocabulary (label governance: labels are part of the API
	// contract).
	ApprovedLabels []string

	// LabelValues documents, for each ApprovedLabel, the allowed value
	// set this submission proposes (label governance: allowed values are
	// part of the contract, not just the label name). Keyed by label
	// name; a label with an open-ended value set (still bounded, just
	// not fully enumerable here) says so in its own entry instead of
	// listing every value.
	LabelValues map[string][]string

	// Cardinality justifies why this metric's label set is safe under
	// the standard's high-cardinality prohibition (no user_id/email/
	// tenant_id/request_id/event_id/session_id or equivalent unbounded
	// value as a label).
	Cardinality string

	// AggregationNotes states how the metric is expected to be queried
	// across services/domains, so cross-service aggregation stays valid.
	AggregationNotes string

	// SupersedesTier3 names the pre-existing Tier 3 metric(s) this
	// service already emits that cover the same signal today and that
	// remain the authoritative alerting/SLO source until Status flips to
	// Canonical. Empty when there is no Tier 3 predecessor.
	SupersedesTier3 []string
}

// PlatformRegistry lists every platform_*/domain-shared metric this
// service itself writes. Their names, help texts, label sets and buckets are
// owned by the central Platform Observability Registry (platform-gincommon
// internal/obsregistry/registry.json) and built from it with
// gincommon.NewShared*Vec (business.go registerShared) — this list mirrors
// each entry's status and records the label values THIS service emits.
// Proposed entries are not yet wired into this repo's alerts, recording
// rules, SLOs, or HPA references; their Tier-3 counterparts stay
// authoritative until real observability governance approves them
// (.github/scripts/metrics-registry-lint.sh). Flip an entry's Status to
// StatusCanonical only once the central registry does.
//
// platform_messages_{received,processed,failed}_total and the outbox /
// SQS / SNS platform_* metrics are written by platform-events, not by this
// service, and so are not listed. The former dependency-errors counter
// submission was withdrawn: it never entered the registry; its concept is
// platform_dependency_request_seconds_count{outcome="error"}.
var PlatformRegistry = []RegistryEntry{
	{
		Name:               "platform_dependency_request_seconds",
		Tier:               "platform",
		Status:             StatusProposed,
		SemanticDefinition: "Client-observed wall time of one call to an external dependency, by outcome. This service records its synchronous REST calls to peer services; platform-events records SNS/SQS/codec calls on the same collector. The _count series counts calls.",
		RequiredLabels:     []string{"domain", "service", "environment"},
		ApprovedLabels:     []string{"dependency", "operation", "outcome"},
		LabelValues: map[string][]string{
			"dependency": {"catalog", "group_mapping", "delegation", "realm_provisioner", "workflow", "token_service"}, // all six synchronous outbound peers (platform-events adds sns, sqs, codec)
			"operation":  {"client endpoint in snake_case: departments/plans (catalog); group_resolution (group_mapping); dept_delegate (delegation); create_invited_user/delete_user/patch_realm_config/revoke_user_sessions/reset_mfa (realm_provisioner); get_delegate_impact/reassign_delegate/cancel_by_delegate (workflow); service_accounts (token_service)"},
			"outcome":    {"success", "error"}, // error = transport failure or 5xx; a 4xx is the caller's own request being rejected
		},
		Cardinality:      "Bounded — dependency × operation is a small, enumerable set (6 peers × a handful of endpoints each) × 2 outcomes; no request/session/tenant identifier is ever a label value.",
		AggregationNotes: "Error ratio per dependency: sum by (domain, service, environment, dependency) (rate(platform_dependency_request_seconds_count{outcome=\"error\"}[w])) / sum by (domain, service, environment, dependency) (rate(platform_dependency_request_seconds_count[w])). p99 latency per dependency: histogram_quantile(0.99, sum by (le, domain, service, environment, dependency) (rate(platform_dependency_request_seconds_bucket[w]))).",
		SupersedesTier3:  []string{"iam_org_membership_dependency_call_duration_seconds", "iam_org_membership_dependency_call_failures_total"},
	},
	{
		Name:               "platform_duplicate_messages_total",
		Tier:               "platform",
		Status:             StatusProposed,
		SemanticDefinition: "A redelivered message acknowledged without running its handler because the consumer's dedup ledger (here: the processed_events table) had already recorded its event ID.",
		RequiredLabels:     []string{"domain", "service", "environment"},
		ApprovedLabels:     []string{"queue", "event_type"},
		LabelValues: map[string][]string{
			"queue":      {"tenant-orgm-q", "billing-orgm-q", "unknown"}, // the two subscribed queues; unknown when no queue is in the context
			"event_type": {"the consumed envelope type (bounded by the event catalogue; unknown when empty)"},
		},
		Cardinality:      "Bounded — 2 queues × the handled event catalogue; no message/event identifier is ever a label value.",
		AggregationNotes: "Platform-wide redelivery-rate board: sum by (domain, service, environment) (rate(platform_duplicate_messages_total[w])).",
		SupersedesTier3:  []string{"iam_org_membership_processed_events_duplicates_total"},
	},
	{
		Name:               "platform_dlq_messages_total",
		Tier:               "platform",
		Status:             StatusCanonical, // approved in the central registry
		SemanticDefinition: "A message moved to dead-letter storage. This service records the consumed events it rejects to the DLQ itself (operation=consume); platform-events records SQS redrive dead-letters (operation=consume) and outbox dead-letters (operation=outbox_publish, reason=max_attempts) on the same collector.",
		RequiredLabels:     []string{"domain", "service", "environment"},
		ApprovedLabels:     []string{"operation", "event_type", "reason"},
		LabelValues: map[string][]string{
			"operation":  {"consume"},
			"event_type": {"the consumed envelope type"},
			"reason":     {"future_time_clamp", "schema_violation"}, // service-side reject reasons
		},
		Cardinality:      "Bounded — 1 operation × the handled event catalogue × 2 reasons (plus platform-events' own values).",
		AggregationNotes: "Dead-letter inflow: sum by (domain, service, environment, operation, reason) (increase(platform_dlq_messages_total[15m])). Any sustained non-zero rate needs attention.",
	},
}
