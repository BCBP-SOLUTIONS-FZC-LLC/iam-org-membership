package domain

import (
	"time"

	"github.com/google/uuid"
)

// SystemActorID is the well-known UUID used as actor_id in system-initiated
// events (cascade deletions, cron jobs). Distinguishable from uuid.Nil.
var SystemActorID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// DomainEvent is the framework-agnostic event carrier passed from services
// into the outbox layer. The eventbus adapter wraps this in an
// events.Envelope[json.RawMessage] and inserts it into outbox_events on
// the transaction TxRunner attached to ctx (EVT-10).
//
// Type is the LLD-canonical PascalCase event name from §7.3
// (e.g. "TenantCreated", "DepartmentMembershipGranted"). The eventbus adapter
// also uses Type to select the destination SNS topic via the local
// RoutingPublisher wrapper (iam.membership.events vs iam.tenant.events).
type DomainEvent struct {
	Type       string
	TenantID   uuid.UUID
	Subject    string // resource identifier the event is about (e.g. user_id, tenant_id)
	Actor      string // user_id of the initiator; "iam-system" for system-driven
	OccurredAt time.Time
	Data       any // payload — JSON-marshalled by the publisher

	// Optional audit fields populated on user-initiated events only
	// (see §7.4 population rule; system/background callers leave both empty).
	IPAddress string
	UserAgent string
}

// Event type constants — the PascalCase LLD-canonical names, referenced from
// service code and pre-fetched into the Glue codec cache at startup.
// See LLD §7.3 for the full outbound event catalogue.
const (
	// iam.membership.events
	EventDepartmentMembershipGranted      = "DepartmentMembershipGranted"
	EventDepartmentMembershipRevoked      = "DepartmentMembershipRevoked"
	EventDepartmentMembershipLevelChanged = "DepartmentMembershipLevelChanged"
	EventTenantRoleGranted                = "TenantRoleGranted"
	EventTenantRoleRevoked                = "TenantRoleRevoked"
	EventTenderAssigneeOverridden         = "TenderAssigneeOverridden"
	// EventMFAReset is P-34's (§16 OQ-8/F6) audit signal — emitted after
	// RealmProvisionerClient.ResetMFA (RP-9) succeeds. O&M persists no MFA
	// state of its own; this event exists solely so the Audit Log consumer
	// (the only real audit mechanism in this codebase, membership-audit-q's
	// catch-all filter) records the actor + target of the reset, per RP's
	// confirmed HLD §8.2.6 flow ("Audit Log records MFAReset with actor and
	// target").
	EventMFAReset                  = "MFAReset"
	EventTenantSeatOverageStarted  = "TenantSeatOverageStarted"
	EventTenantSeatOverageResolved = "TenantSeatOverageResolved"
	EventTenantStateChanged        = "TenantStateChanged"
	// EventMembershipRevoked replaces two cascades RemoveUser/DeleteMember
	// used to perform synchronously, in the same DB transaction, via
	// DelegationRepository.SoftDeleteForUser and
	// TenderACLRepository.SoftDeleteForUser (P-8/I-5). Once `delegations`
	// and `tender_acl_entries` moved into the Delegation Service's and
	// Tender-ACL Service's own databases those same-transaction calls
	// became impossible; per LLD §15.2.2 both services' consumers subscribe
	// to this single shared event instead and run their own asynchronous
	// cascades (ADR-0008 §6.4). Emitted unconditionally, not gated on
	// whether the removed user actually held any delegation/ACL rows —
	// both consumers are idempotent regardless.
	EventMembershipRevoked = "MembershipRevoked"

	// EventTenantMembershipsPurged is Core's own tenant-level cascade
	// signal on a real suspended/whatever→offboarded transition
	// (EVT-16-adjacent — emitted only on the real transition, from the
	// same tx as the projection UPDATE). The Delegation, Tender-ACL, and
	// Group-Mapping services consume this to run their own asynchronous
	// tenant-scoped cascade-deletes (LLD §15.5, ADR-0008 §6.4 pattern).
	// Distinct from the generic EventTenantStateChanged relay (which also
	// fires on this transition) — this one exists specifically so a
	// downstream service can filter on it without matching every other
	// status/plan change. Lives on iam.membership.events, NOT
	// iam.tenant.events — Core never re-emits the Realm-Provisioner-
	// produced `TenantOffboarded` event it only consumes (LLD §16 OQ-1:
	// "one producer per event name").
	EventTenantMembershipsPurged = "TenantMembershipsPurged"

	// iam.tenant.events
	EventTenantCreated = "TenantCreated"
	EventTrialStarted  = "TrialStarted"
)

// TopicMembership and TopicTenant identify the two SNS topics O&M publishes
// to. The eventbus RoutingPublisher uses TopicForEvent to select between them.
const (
	TopicMembership = "iam.membership.events"
	TopicTenant     = "iam.tenant.events"
)

// TopicForEvent returns the destination topic for a given event Type per
// §7.3. Unknown types default to TopicMembership (the catch-all lane) — the
// eventbus wraps this call and the validating codec would have rejected an
// unknown Type before it reached this point.
func TopicForEvent(eventType string) string {
	switch eventType {
	case EventTenantCreated, EventTrialStarted:
		return TopicTenant
	default:
		return TopicMembership
	}
}

// IsProducedEvent reports whether eventType is one of the 14 events this
// service publishes (and therefore registers in Glue). schema-gov extract
// also writes the 13 consumed, producer-owned payloads into the same
// schema directory for coverage; those must not be prefetched or
// registered here.
func IsProducedEvent(eventType string) bool {
	switch eventType {
	case EventDepartmentMembershipGranted,
		EventDepartmentMembershipRevoked,
		EventDepartmentMembershipLevelChanged,
		EventTenantRoleGranted,
		EventTenantRoleRevoked,
		EventTenderAssigneeOverridden,
		EventMFAReset,
		EventTenantSeatOverageStarted,
		EventTenantSeatOverageResolved,
		EventTenantStateChanged,
		EventMembershipRevoked,
		EventTenantMembershipsPurged,
		EventTenantCreated,
		EventTrialStarted:
		return true
	default:
		return false
	}
}
