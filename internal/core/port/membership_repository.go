package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
)

// MembershipRepository owns tenant_memberships lifecycle (§16 A14 —
// no role data). All operations RLS-scoped.
type MembershipRepository interface {
	// List returns a keyset-paginated page of active memberships.
	List(ctx context.Context, tenantID uuid.UUID, cursor *domain.MembershipListCursor, limit int) (*domain.MembershipListPage, error)

	// FindByUserID returns the active tenant_memberships row for a user
	// within the RLS-scoped tenant. Returns ErrMemberNotFound if none.
	FindByUserID(ctx context.Context, tenantID, userID uuid.UUID) (*domain.TenantMembership, error)

	// Insert creates a new membership row. Used by acceptance (I-3) and by
	// admin-side flows in Phase 4.
	Insert(ctx context.Context, tm *domain.TenantMembership) (*domain.TenantMembership, error)

	// SetStatus flips the status field with optimistic locking. Used by
	// P-7 (suspend/reactivate) and by I-4 (Keycloak lifecycle).
	SetStatus(ctx context.Context, tenantID, userID uuid.UUID, status domain.MembershipStatus, expectedVersion int64) (*domain.TenantMembership, error)

	// SoftDelete marks the membership deleted (TM-5 GDPR wipe path).
	// Cascades on child rows are enforced at the FK layer.
	SoftDelete(ctx context.Context, tenantID, userID uuid.UUID, expectedVersion int64) error

	// CountActive returns the number of active (non-deleted, non-left)
	// memberships. Used by SEAT-1 under FOR UPDATE on tenants.
	CountActive(ctx context.Context, tenantID uuid.UUID) (int, error)

	// ListActiveUserIDs returns the user_id of every non-deleted membership
	// for the tenant. Used by O-4 to evict per-user I-8 cache entries when
	// feature_flags change (CACHE-3).
	ListActiveUserIDs(ctx context.Context, tenantID uuid.UUID) ([]uuid.UUID, error)
}

// TenantRoleRepository owns elevated role grants on tenant_roles.
type TenantRoleRepository interface {
	// ListByUser returns all active elevated roles for a user.
	ListByUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error)

	// ListByRole returns all users holding a given elevated role
	// (used by TM-8 last-owner guard when role=tenant_owner).
	ListByRole(ctx context.Context, tenantID uuid.UUID, role domain.TenantRoleCode) ([]domain.TenantRole, error)

	// CountActiveOwners is a hot-path count for TM-8 last-owner guard.
	CountActiveOwners(ctx context.Context, tenantID uuid.UUID) (int, error)

	// Grant creates an elevated role row. chk_tr_no_member enforces the
	// role is not 'member' at DB level.
	Grant(ctx context.Context, r *domain.TenantRole) (*domain.TenantRole, error)

	// Revoke soft-deletes an active role row.
	Revoke(ctx context.Context, tenantID, userID uuid.UUID, role domain.TenantRoleCode) (*domain.TenantRole, error)

	// SoftDeleteAllForUser cascades revocation across every elevated role
	// held by a user (called on membership removal cascade, TR-9).
	SoftDeleteAllForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.TenantRole, error)
}

// DeptMembershipRepository owns dept_memberships (user × dept × level).
type DeptMembershipRepository interface {
	ListByUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.DeptMembership, error)
	ListByDepartment(ctx context.Context, tenantID, departmentID uuid.UUID) ([]domain.DeptMembership, error)

	// Assign upserts a (user, dept) pair to the given level. On level
	// change it soft-deletes the old row and inserts a new one so the
	// DepartmentMembershipLevelChanged event carries previous_level.
	//
	// Returns (current, previous, err) — previous is nil when no active row
	// existed before this call (fresh grant). previous is determined
	// atomically inside the same lock as the write (B15) — callers must use
	// it instead of a separate pre-fetch to decide Granted/LevelChanged/no-op.
	Assign(ctx context.Context, tenantID, userID, departmentID uuid.UUID, membershipID uuid.UUID, level domain.DeptRole, grantedBy uuid.UUID) (current *domain.DeptMembership, previous *domain.DeptMembership, err error)

	// Remove soft-deletes an active dept membership.
	Remove(ctx context.Context, tenantID, userID, departmentID uuid.UUID) (*domain.DeptMembership, error)

	// SoftDeleteAllForUser is called on membership removal (§8.8 cascade).
	SoftDeleteAllForUser(ctx context.Context, tenantID, userID uuid.UUID) ([]domain.DeptMembership, error)

	// SoftDeleteAllForDept soft-deletes every active dept_membership row for
	// the given (tenant, department) pair. Used by admin tooling / future flows.
	SoftDeleteAllForDept(ctx context.Context, tenantID, departmentID uuid.UUID) ([]domain.DeptMembership, error)
}

// DeptRoleLabelRepository owns per-tenant display labels for dept_role.
type DeptRoleLabelRepository interface {
	List(ctx context.Context, tenantID uuid.UUID) ([]domain.DeptRoleLabel, error)
	Update(ctx context.Context, tenantID uuid.UUID, roleCode domain.DeptRole, displayName string, expectedVersion int64) (*domain.DeptRoleLabel, error)
	Seed(ctx context.Context, tenantID uuid.UUID) ([]domain.DeptRoleLabel, error) // called at trial provisioning
}
