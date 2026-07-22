package domain

import (
	"time"

	"github.com/google/uuid"
)

// DelegationScope mirrors the delegation_scope enum (§4.1).
type DelegationScope string

const (
	ScopeAll        DelegationScope = "all"
	ScopeDepartment DelegationScope = "department"
	ScopeTender     DelegationScope = "tender"
)

// DelegationStatus mirrors the delegation_status enum (§4.1).
type DelegationStatus string

const (
	DelegationActive    DelegationStatus = "active"
	DelegationEnded     DelegationStatus = "ended"
	DelegationCancelled DelegationStatus = "cancelled"
)

// EndReason is the payload field emitted with DelegationEnded (DEL-7).
// Not persisted — event-payload only.
type EndReason string

const (
	EndReasonExpired         EndReason = "expired"
	EndReasonCancelled       EndReason = "cancelled"
	EndReasonDelegateRemoved EndReason = "delegate_removed"
)

// Delegation is the authoritative OOO grant that drives workflow reroute
// via DelegationStarted / DelegationEnded events on iam.membership.events.
// User Profile keeps a presentation-only user_availability row (soft
// pointer). See §8.6 / §8.7 for the availability-first coordination flow.
type Delegation struct {
	ID                    uuid.UUID
	TenantID              uuid.UUID
	DelegatorID           uuid.UUID
	DelegateID            uuid.UUID
	DelegatorMembershipID uuid.UUID // composite FK (§16 A16, DEL-9)
	DelegateMembershipID  uuid.UUID
	Scope                 DelegationScope
	ScopeID               *uuid.UUID // required when Scope != all (DEL-2)
	Reason                string     // audit-only, capped 500 chars service-side (§16 A32(f), DEL-10)
	StartsAt              time.Time
	EndsAt                *time.Time // open-ended allowed (DEL-8)
	Status                DelegationStatus
	RecordVersion         int64
	CreatedAt             time.Time
	UpdatedAt             time.Time
	DeletedAt             *time.Time
}
