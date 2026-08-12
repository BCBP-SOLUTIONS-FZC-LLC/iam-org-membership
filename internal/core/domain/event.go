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
// events.Envelope[json.RawMessage] and inserts it into outbox_events within
// the caller's active pgx.Tx (EVT-10).
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
	EventDelegationStarted                = "DelegationStarted"
	EventDelegationEnded                  = "DelegationEnded"
	EventDelegationReviewRequested        = "DelegationReviewRequested"
	EventTenderAssigneeOverridden         = "TenderAssigneeOverridden"
	EventTenantSeatOverageStarted         = "TenantSeatOverageStarted"
	EventTenantSeatOverageResolved        = "TenantSeatOverageResolved"
	EventTenantStateChanged               = "TenantStateChanged"

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
