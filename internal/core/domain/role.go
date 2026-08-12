package domain

import (
	"time"

	"github.com/google/uuid"
)

// TenantRoleCode mirrors the tenant_role enum (§4.1).
// 'member' is DERIVED-ONLY per TR-7 / §16 A29 — never persisted. Included
// in the enum so header wire values and derived value share a domain.
type TenantRoleCode string

const (
	RoleTenantOwner TenantRoleCode = "tenant_owner"
	RoleTenantAdmin TenantRoleCode = "tenant_admin"
	RoleTenderAdmin TenantRoleCode = "tender_admin"
	RoleMember      TenantRoleCode = "member" // derived-only (TR-7)
)

// IsElevated reports whether the role represents an elevated grant that
// gets a row in tenant_roles. 'member' is the sole non-elevated code and
// is barred from persistence by chk_tr_no_member.
func (c TenantRoleCode) IsElevated() bool {
	return c == RoleTenantOwner || c == RoleTenantAdmin || c == RoleTenderAdmin
}

// TenantRole is a single elevated role grant. Multi-role per user allowed
// via one row per (tenant_id, user_id, role_code) — TR-1.
type TenantRole struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	UserID             uuid.UUID
	TenantMembershipID uuid.UUID      // composite FK anchor (§16 A31, TR-8)
	RoleCode           TenantRoleCode // chk_tr_no_member enforces 'member' cannot be here
	GrantedBy          uuid.UUID      // audit
	RecordVersion      int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
	DeletedAt          *time.Time // soft-delete on revoke
}

// DeptRole mirrors the dept_role enum (§4.1).
type DeptRole string

const (
	DeptPreparator DeptRole = "preparator"
	DeptReviewer   DeptRole = "reviewer"
	DeptApprover   DeptRole = "approver"
)

// Rank orders the three dept role levels for privilege comparison.
// Higher rank = more privileged (preparator=1, reviewer=2, approver=3).
// Used by WFI-9/WFI-12 to detect level decreases in Assign.
func (r DeptRole) Rank() int {
	switch r {
	case DeptPreparator:
		return 1
	case DeptReviewer:
		return 2
	case DeptApprover:
		return 3
	}
	return -1
}

// Satisfies reports whether this role meets or exceeds the required minimum
// (LLD §5.4 I-13 step 2: assignee's role_level >= required_level).
func (r DeptRole) Satisfies(minimum DeptRole) bool {
	rr, mr := r.Rank(), minimum.Rank()
	if rr < 0 || mr < 0 {
		return false
	}
	return rr >= mr
}

// DeptMembership is a user × department × role_level assignment.
// tenant_membership_id composite-FKs to tenant_memberships (§16 A15/A28,
// DM-4).
type DeptMembership struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	UserID             uuid.UUID
	TenantMembershipID uuid.UUID
	DepartmentID       uuid.UUID
	RoleLevel          DeptRole
	GrantedBy          uuid.UUID // 'iam-system' UUID for JIT (§16 A32(e), DM-5)
	RecordVersion      int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
	DeletedAt          *time.Time
}

// DeptRoleLabel is a per-tenant customizable display label for one of the
// three DeptRole values (§4.2, DRL-1). Three rows per tenant, seeded at
// provisioning.
type DeptRoleLabel struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	RoleCode      DeptRole
	DisplayName   string
	RecordVersion int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
