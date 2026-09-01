package domain

import (
	"time"

	"github.com/google/uuid"
)

// MembershipStatus mirrors the membership_status enum (§4.1).
type MembershipStatus string

const (
	MembershipActive    MembershipStatus = "active"
	MembershipSuspended MembershipStatus = "suspended"
	MembershipLeft      MembershipStatus = "left"
)

// TenantMembership is lifecycle-only per §16 A14 — carries no role data.
// The uq_tm_id_tenant_user composite unique is the FK target for
// tenant_roles / dept_memberships (§16 A15/A16/A28/A31). delegations and
// tender_acl_entries moved to their own services' databases (ADR-0007/0008)
// and reference this row via the synchronous I-15 existence check instead
// of a cross-database FK (§19.2).
type TenantMembership struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	UserID        uuid.UUID // Keycloak sub
	Status        MembershipStatus
	RecordVersion int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     *time.Time // GDPR wipe only (TM-5)
}

// MembershipListCursor is the keyset-pagination cursor for P-4 (§21.2,
// idx_tm_tenant_created is the seek index). Encoded as opaque base64 on
// the wire.
type MembershipListCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// MembershipListPage is the P-4 response envelope.
type MembershipListPage struct {
	Items      []MembershipListItem
	NextCursor *MembershipListCursor
}

// MembershipListItem is the projection returned by P-4 — joins the
// membership row with elevated tenant_roles.role_code[] and any active
// dept_memberships. Display fields (display_name, email) are hydrated
// downstream via User Profile's batch endpoint per HLD §5.6.
type MembershipListItem struct {
	Membership  TenantMembership
	TenantRoles []TenantRoleCode // includes derived "member" per TR-7
	Departments []DeptMembershipView
}

// DeptMembershipView is the compact per-user dept view embedded in the
// membership projection. Code is populated only by I-8 (AuthZService looks
// it up via the om:departments cache, LLD §5.4) — populated with the empty
// string wherever a caller (e.g. P-4, which copies these fields into its
// own DeptMemberView DTO) doesn't have a department catalog reader at hand.
type DeptMembershipView struct {
	DepartmentID uuid.UUID `json:"department_id"`
	Code         string    `json:"code,omitempty"`
	RoleLevel    DeptRole  `json:"role_level"`
}
