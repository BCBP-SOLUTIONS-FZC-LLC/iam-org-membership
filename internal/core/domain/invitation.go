package domain

import (
	"time"

	"github.com/google/uuid"
)

// InvitationStatus mirrors the invitation_status enum (§4.1, §16 A11).
type InvitationStatus string

const (
	InvitePending  InvitationStatus = "pending"
	InviteAccepted InvitationStatus = "accepted"
	InviteExpired  InvitationStatus = "expired"
	InviteRevoked  InvitationStatus = "revoked"
)

// PendingInvitation is the two-step invite→accept staging row (§4.2, §16 A11).
// SOLE PII exception in the O&M schema: carries email + full_name until
// acceptance (§15.8).
type PendingInvitation struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	Email               string // citext in DB (case-insensitive)
	FullName            string
	InitialTenantRoles  []TenantRoleCode
	InitialDeptMappings []InvitationDeptMapping // stored as JSONB
	InvitedBy           uuid.UUID               // Keycloak sub of inviting admin
	KeycloakUserID      *uuid.UUID              // set post-RP-CreateInvitedUser
	Status              InvitationStatus
	ExpiresAt           time.Time // 7d default (INVITATION_EXPIRY_DAYS)
	AcceptedAt          *time.Time
	KCCleanupPending    bool // §16 A34, PI-9
	RecordVersion       int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// InvitationDeptMapping is one entry inside the initial_dept_mappings jsonb.
type InvitationDeptMapping struct {
	DepartmentID uuid.UUID `json:"department_id"`
	Level        DeptRole  `json:"level"`
}

// SeatUsage is the P-27 / I-11 response envelope. Computed under FOR UPDATE
// inside SEAT-1's transactional cap check (§8.10, SEAT-1..5).
type SeatUsage struct {
	TenantID           uuid.UUID
	ActiveUsers        int
	PendingInvitations int
	LicensedSeats      int
	OverCap            bool       // active + pending > licensed_seats
	OverageSince       *time.Time // SEAT-5 marker
	GraceEndsAt        *time.Time // OverageSince + SEAT_OVERAGE_GRACE_DAYS
}
