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
// port.EventPublisher via Enqueue. The eventbus adapter wraps them in
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

// MembershipRevokedPayload is EventMembershipRevoked's data — deliberately
// minimal (just enough for a consumer to scope a DELETE/UPDATE ... WHERE
// tenant_id = $1 AND user_id = $2 against its own tenant-scoped rows).
// Per LLD §15.2.2, this single event is shared by both the Delegation
// Service's delegation-cascade-q consumer (ends the user's delegation rows,
// mirroring what DelegationRepository.SoftDeleteForUser used to do
// in-process) and the Tender-ACL Service's equivalent consumer (soft-
// deletes the user's ACL overlays, mirroring
// TenderACLRepository.SoftDeleteForUser) — ADR-0008 §6.4.
type MembershipRevokedPayload struct {
	TenantID uuid.UUID `json:"tenant_id"`
	UserID   uuid.UUID `json:"user_id"`
	ActorID  uuid.UUID `json:"actor_id"`
}

type TenderAssigneeOverriddenPayload struct {
	TenderID uuid.UUID `json:"tender_id"`
	TenantID uuid.UUID `json:"tenant_id"`
	UserID   uuid.UUID `json:"user_id"`
	ActorID  uuid.UUID `json:"actor_id"`
}

// MFAResetPayload is EventMFAReset's data (§16 OQ-8/F6, P-34) — the audit
// record RP's confirmed HLD §8.2.6 flow requires ("Audit Log records
// MFAReset with actor and target"). O&M persists no MFA state itself; this
// payload exists solely for the Audit Log consumer.
type MFAResetPayload struct {
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

// TenantMembershipsPurgedPayload is EventTenantMembershipsPurged's data —
// Core's own tenant-level cascade signal on a real
// suspended/whatever→offboarded transition (see
// membership_event_consumer.go's Handle, right next to the generic EVT-16
// TenantStateChanged relay this mirrors). Consumed by the Delegation,
// Tender-ACL, and Group-Mapping services to run their own asynchronous
// tenant-scoped cascade-deletes (LLD §15.5, ADR-0008 §6.4 pattern,
// ADR-0007 §12). Distinct from — and never re-emits — the Realm-
// Provisioner-produced `TenantOffboarded` event on iam.tenant.events,
// which Core only ever consumes (LLD §16 OQ-1: "Core does not re-emit
// TenantOffboarded" — one producer per event name).
type TenantMembershipsPurgedPayload struct {
	TenantID uuid.UUID `json:"tenant_id"`
	ActorID  uuid.UUID `json:"actor_id"`
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
