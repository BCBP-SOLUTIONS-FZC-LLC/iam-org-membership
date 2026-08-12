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
// tenant_roles / dept_memberships / delegations / tender_acl_entries
// (§16 A15/A16/A28/A31).
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
// membership projection.
type DeptMembershipView struct {
	DepartmentID uuid.UUID `json:"department_id"`
	RoleLevel    DeptRole  `json:"level"`
}
