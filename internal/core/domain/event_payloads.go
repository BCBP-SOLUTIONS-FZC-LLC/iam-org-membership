package domain

import (
	"time"

	"github.com/google/uuid"
)

// Event payload structs — one per outbound event (§7.3). These become the
// `data` field of the CloudEvents envelope. JSON tags match the field
// names in the LLD's schema tables.
//
// Producer contract: services fill these and hand them to
// port.EventPublisher via EnqueueCtx. The eventbus adapter wraps them in
// an events.Envelope[json.RawMessage] and inserts into outbox_events
// atomically with the business tx (EVT-10, CONS-1..4).

// ── iam.membership.events ──────────────────────────────────────────────

type DepartmentMembershipGrantedPayload struct {
	UserID       uuid.UUID `json:"user_id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	DepartmentID uuid.UUID `json:"department_id"`
	Level        DeptRole  `json:"level"`
	ActorID      uuid.UUID `json:"actor_id"`
}

type DepartmentMembershipRevokedPayload struct {
	UserID       uuid.UUID `json:"user_id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	DepartmentID uuid.UUID `json:"department_id"`
	ActorID      uuid.UUID `json:"actor_id"`
}

type DepartmentMembershipLevelChangedPayload struct {
	UserID        uuid.UUID `json:"user_id"`
	TenantID      uuid.UUID `json:"tenant_id"`
	DepartmentID  uuid.UUID `json:"department_id"`
	PreviousLevel DeptRole  `json:"previous_level"`
	NewLevel      DeptRole  `json:"new_level"`
	ActorID       uuid.UUID `json:"actor_id"`
}

type TenantRoleGrantedPayload struct {
	UserID   uuid.UUID      `json:"user_id"`
	TenantID uuid.UUID      `json:"tenant_id"`
	RoleCode TenantRoleCode `json:"role_code"`
	ActorID  uuid.UUID      `json:"actor_id"`
}

type TenantRoleRevokedPayload struct {
	UserID   uuid.UUID      `json:"user_id"`
	TenantID uuid.UUID      `json:"tenant_id"`
	RoleCode TenantRoleCode `json:"role_code"`
	ActorID  uuid.UUID      `json:"actor_id"`
}

type DelegationStartedPayload struct {
	DelegationID uuid.UUID       `json:"delegation_id"`
	TenantID     uuid.UUID       `json:"tenant_id"`
	DelegatorID  uuid.UUID       `json:"delegator_id"`
	DelegateID   uuid.UUID       `json:"delegate_id"`
	Scope        DelegationScope `json:"scope"`
	ScopeID      *uuid.UUID      `json:"scope_id,omitempty"`
	EndsAt       *time.Time      `json:"ends_at,omitempty"`
	ActorID      uuid.UUID       `json:"actor_id"`
}

type DelegationEndedPayload struct {
	DelegationID uuid.UUID       `json:"delegation_id"`
	TenantID     uuid.UUID       `json:"tenant_id"`
	DelegatorID  uuid.UUID       `json:"delegator_id"`
	DelegateID   uuid.UUID       `json:"delegate_id"`
	Scope        DelegationScope `json:"scope"`
	ScopeID      *uuid.UUID      `json:"scope_id,omitempty"`
	EndedReason  EndReason       `json:"ended_reason"` // expired | cancelled | delegate_removed (DEL-7)
	ActorID      uuid.UUID       `json:"actor_id"`
}

type TenderAssigneeOverriddenPayload struct {
	TenderID uuid.UUID `json:"tender_id"`
	TenantID uuid.UUID `json:"tenant_id"`
	UserID   uuid.UUID `json:"user_id"`
	ActorID  uuid.UUID `json:"actor_id"`
}

type TenantSeatOverageStartedPayload struct {
	TenantID           uuid.UUID `json:"tenant_id"`
	LicensedSeats      int       `json:"licensed_seats"`
	ActiveUsers        int       `json:"active_users"`
	PendingInvitations int       `json:"pending_invitations"`
	OverageSince       time.Time `json:"overage_since"`
}

type TenantSeatOverageResolvedPayload struct {
	TenantID   uuid.UUID `json:"tenant_id"`
	ResolvedAt time.Time `json:"resolved_at"`
}

// TenantStateChangedPayload is the §16 A61 relay emitted whenever a
// consumed lifecycle event actually changes tenants.status or tenants.plan
// (EVT-16). Never emitted on stale-skip or no-op.
type TenantStateChangedPayload struct {
	TenantID       uuid.UUID          `json:"tenant_id"`
	Status         SubscriptionStatus `json:"status"`
	PreviousStatus SubscriptionStatus `json:"previous_status"`
	Plan           TenantPlan         `json:"plan"`
	PreviousPlan   TenantPlan         `json:"previous_plan"`
	ChangedAt      time.Time          `json:"changed_at"`
	Cause          string             `json:"cause"` // event type that caused the transition
}

// ── iam.tenant.events ──────────────────────────────────────────────────

type TenantCreatedPayload struct {
	TenantID uuid.UUID          `json:"tenant_id"`
	Slug     string             `json:"slug"`
	Plan     TenantPlan         `json:"plan"`
	Status   SubscriptionStatus `json:"status"`
}

type TrialStartedPayload struct {
	TenantID    uuid.UUID  `json:"tenant_id"`
	Plan        TenantPlan `json:"plan"`
	TrialEndsAt time.Time  `json:"trial_ends_at"`
}
