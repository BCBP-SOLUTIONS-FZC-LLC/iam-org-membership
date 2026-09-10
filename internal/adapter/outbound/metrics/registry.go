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
// service emits or has proposed. All three entries here are this
// service's own Registry Ratification Requirement submissions — none is
// yet Canonical, so none is wired into this repo's alerts, recording
// rules, SLOs, or HPA references (see each metric's doc comment in
// business.go and every alert/SLO file's header comment for the same
// note). Flip an entry's Status to StatusCanonical only once real
// observability governance has actually approved it, and only then
// proceed with the standard's Backward Compatibility migration steps
// (dashboards → alerts → recording rules → SLOs → HPA → deprecate →
// remove after sunset).
var PlatformRegistry = []RegistryEntry{
	{
		Name:               "platform_dependency_request_seconds",
		Tier:               "platform",
		Status:             StatusProposed,
		SemanticDefinition: "Latency of one outbound call this service makes to a dependency it calls synchronously (a REST/gRPC/admin-API call — not a queue send/receive, which platform-events' own events_* metrics already cover). Named explicitly in the standard's own Registry-Proposed Examples.",
		RequiredLabels:     []string{"domain", "service", "environment"},
		ApprovedLabels:     []string{"target_service", "endpoint"},
		LabelValues: map[string][]string{
			"target_service": {"catalog", "group_mapping", "delegation"}, // the three ADR-0007/ADR-0008 synchronous outbound peers this service calls
			"endpoint":       {"open — the catalogadmin/groupmappingclient/delegationcheck client method names this service already uses as its `endpoint` label today, e.g. GET /internal/plans, GET /internal/departments, POST /internal/tenants/{id}/group-resolution, GET /internal/delegations/dept-delegate"},
		},
		Cardinality:      "Bounded — target_service × endpoint is a small, enumerable set (3 peers × a handful of endpoints each); no request/session/tenant identifier is ever a label value.",
		AggregationNotes: "Per-dependency failure rate: sum(rate(platform_dependency_request_seconds_count{target_service=\"X\"}[w])) joined against platform_dependency_errors_total. Cross-domain dependency-health board: sum by (domain, target_service) rate(..._count[w]). p99 latency per dependency: histogram_quantile(0.99, sum by (le, target_service) (rate(..._bucket[w]))).",
		SupersedesTier3:  []string{"iam_org_membership_dependency_call_duration_seconds"},
	},
	{
		Name:               "platform_duplicate_messages_total",
		Tier:               "platform",
		Status:             StatusProposed,
		SemanticDefinition: "A message a consumer received that its own dedup store (however implemented — a processed-events table, an idempotency cache, etc.) had already marked processed: a redelivery, not a new logical message. Independent of transport (SQS/SNS/Kafka) or domain. Named explicitly in the standard's own Registry-Proposed Examples.",
		RequiredLabels:     []string{"domain", "service", "environment"},
		ApprovedLabels:     []string{"consumer"},
		LabelValues: map[string][]string{
			"consumer": {"membership"}, // this service's single MembershipEventConsumer, shared by tenant-orgm-q and billing-orgm-q
		},
		Cardinality:      "Bounded — one value per consumer this service registers (currently 1); no message/event identifier is ever a label value.",
		AggregationNotes: "Platform-wide redelivery-rate board: sum by (domain, service) rate(platform_duplicate_messages_total[w]). A sustained nonzero rate across many unrelated services signals a shared upstream cause (e.g. a producer retry storm or a broker redelivery-timeout misconfiguration), not a per-service bug — the reason this is worth a platform_* name rather than staying per-service.",
		SupersedesTier3:  []string{"iam_org_membership_processed_events_duplicates_total"},
	},
	{
		Name:               "platform_dependency_errors_total",
		Tier:               "platform",
		Status:             StatusProposed,
		SemanticDefinition: "A synchronous cross-service dependency call that failed, classified by outcome (5xx|timeout|fallback_served — the last recorded by the calling service layer, not the client, since only the caller knows whether a stale/last-known-good value was served instead of surfacing the error). NOT one of the standard's own named canonical/registry-proposed examples — added by symmetry with platform_dependency_request_seconds (rule 7: prefer shared whenever reusable) since outcome-classified failure counts aren't recoverable from a latency histogram's buckets alone. This is why this entry needs its own justification distinct from the two above, which the standard already names as anticipated candidates.",
		RequiredLabels:     []string{"domain", "service", "environment"},
		ApprovedLabels:     []string{"target_service", "endpoint", "outcome"},
		LabelValues: map[string][]string{
			"target_service": {"catalog", "group_mapping", "delegation"},
			"endpoint":       {"open — see platform_dependency_request_seconds's endpoint entry"},
			"outcome":        {"5xx", "timeout", "fallback_served"},
		},
		Cardinality:      "Bounded — target_service × endpoint × outcome is a small, enumerable set; no request/session/tenant identifier is ever a label value.",
		AggregationNotes: "increase(platform_dependency_errors_total{target_service=\"catalog\"}[5m]) > 0 warns regardless of which service emits it — the intended cross-service alert shape once ratified (catalog is this service's one NOT-fail-open dependency, §20.7). Platform-wide dependency-health board: sum by (domain, service, target_service, outcome) rate(...[w]).",
		SupersedesTier3:  []string{"iam_org_membership_dependency_call_failures_total"},
	},
}
