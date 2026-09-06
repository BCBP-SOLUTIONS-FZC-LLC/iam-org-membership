//go:build integration

// dept_role_error_paths_test.go — targeted integration tests for error paths
// in DeptMembershipRepository, DeptRoleLabelRepository, and TenantRoleRepository.
//
// Uncovered lines targeted:
//
//	dept_membership_repository.go:  48,54,82,94,104,119,127,132,135,154,172,178,204,210
//	dept_role_label_repository.go:  40,46,69,70,73,78,99
//	tenant_role_repository.go:      94,107,129,145,151
//
// Tests are grouped into 3 parent functions, each sharing ONE testcontainer.
// Subtests run in parallel within each group to keep wall-clock time
// manageable on CI runners without sacrificing coverage or data isolation.
//
// Techniques:
//  1. Context cancellation  → triggers query/exec errors on any DB call.
//  2. Non-existent UUIDs   → ErrNoRows / ErrMemberNotFound paths.
//  3. Same-level re-Assign → no-op idempotent return (line 104).
//  4. Level-change re-Assign → soft-delete old + insert new (lines 119,132).
//  5. ON CONFLICT winner   → rawPool pre-insert + Assign (lines 127,135).
//  6. Wrong record_version → optimistic-lock conflict (lines 69-73,78).
package postgres_test

import (
	"context"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═════════════════════════════════════════════════════════════════════════════
// DeptMembershipRepository — error paths and branch coverage
// (one shared container, 9 subtests)
// ═════════════════════════════════════════════════════════════════════════════

func TestDRepo_DeptMembership(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)

	// DREPO-DM-001 — ListByUser ctx cancelled (lines 48,54)
	t.Run("ListByUser_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "dm-listbyuser-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewDeptMembershipRepository(appPool)
		_, err := repo.ListByUser(cancelCtx, tenantID, uuid.New())
		assert.Error(t, err, "cancelled context must return an error from ListByUser")
	})

	// DREPO-DM-002 — ListByDepartment ctx cancelled (lines 48,54)
	t.Run("ListByDepartment_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "dm-listbydept-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewDeptMembershipRepository(appPool)
		_, err := repo.ListByDepartment(cancelCtx, tenantID, uuid.New())
		assert.Error(t, err, "cancelled context must return an error from ListByDepartment")
	})

	// DREPO-DM-003 — Assign ctx cancelled at advisory lock (line 82)
	t.Run("Assign_CtxCancelled_AdvisoryLock", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "dm-assign-ctx-advisory")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewDeptMembershipRepository(appPool)
		_, _, err := repo.Assign(cancelCtx, tenantID, uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())
		assert.Error(t, err, "cancelled context must return an error during advisory lock")
	})

	// DREPO-DM-004 — Assign same level → idempotent no-op (lines 94,104)
	t.Run("Assign_SameLevel_NoOp", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "dm-assign-samelevel")
		userID := uuid.New()
		deptID := uuid.New()
		grantedBy := uuid.New()

		_, err := rawPool.Exec(ctx,
			`INSERT INTO tenant_departments (tenant_id, department_id, is_active) VALUES ($1, $2, true)`,
			tenantID, deptID)
		require.NoError(t, err)

		memID := uuid.New()
		_, err = rawPool.Exec(ctx,
			`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
			memID, tenantID, userID)
		require.NoError(t, err)

		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewDeptMembershipRepository(appPool)

		first, prev1, err := repo.Assign(tctx, tenantID, userID, deptID, memID, domain.DeptPreparator, grantedBy)
		require.NoError(t, err)
		assert.NotNil(t, first)
		assert.Nil(t, prev1, "fresh grant must have nil previous")
		assert.Equal(t, domain.DeptPreparator, first.RoleLevel)

		second, prev2, err := repo.Assign(tctx, tenantID, userID, deptID, memID, domain.DeptPreparator, grantedBy)
		require.NoError(t, err)
		assert.NotNil(t, second)
		assert.NotNil(t, prev2, "idempotent re-assign must report previous (existing) row")
		assert.Equal(t, first.ID, second.ID, "no-op must return the SAME row ID")
		assert.Equal(t, domain.DeptPreparator, second.RoleLevel)
	})

	// DREPO-DM-005 — Assign level change soft-deletes old + inserts new (lines 119,132)
	t.Run("Assign_LevelChange", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "dm-assign-levelchange")
		userID := uuid.New()
		deptID := uuid.New()
		grantedBy := uuid.New()

		_, err := rawPool.Exec(ctx,
			`INSERT INTO tenant_departments (tenant_id, department_id, is_active) VALUES ($1, $2, true)`,
			tenantID, deptID)
		require.NoError(t, err)

		memID := uuid.New()
		_, err = rawPool.Exec(ctx,
			`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
			memID, tenantID, userID)
		require.NoError(t, err)

		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewDeptMembershipRepository(appPool)

		first, _, err := repo.Assign(tctx, tenantID, userID, deptID, memID, domain.DeptPreparator, grantedBy)
		require.NoError(t, err)
		require.NotNil(t, first)
		assert.Equal(t, domain.DeptPreparator, first.RoleLevel)

		second, prevRow, err := repo.Assign(tctx, tenantID, userID, deptID, memID, domain.DeptReviewer, grantedBy)
		require.NoError(t, err)
		require.NotNil(t, second)
		require.NotNil(t, prevRow, "level change must return previous row")
		assert.Equal(t, domain.DeptReviewer, second.RoleLevel, "new row must carry the updated level")
		assert.NotEqual(t, first.ID, second.ID, "level change must produce a new row ID")
		assert.Equal(t, domain.DeptPreparator, prevRow.RoleLevel, "previous must carry the old level")

		var deletedAt *string
		err = rawPool.QueryRow(ctx,
			`SELECT deleted_at::text FROM dept_memberships WHERE id = $1`, first.ID).Scan(&deletedAt)
		require.NoError(t, err)
		assert.NotNil(t, deletedAt, "old dept_membership must be soft-deleted after level change")
	})

	// DREPO-DM-006 — Assign ON CONFLICT winner path (lines 127,135)
	t.Run("Assign_ConflictWinner", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "dm-assign-conflict-winner")
		userID := uuid.New()
		deptID := uuid.New()
		grantedBy := uuid.New()

		_, err := rawPool.Exec(ctx,
			`INSERT INTO tenant_departments (tenant_id, department_id, is_active) VALUES ($1, $2, true)`,
			tenantID, deptID)
		require.NoError(t, err)

		memID := uuid.New()
		_, err = rawPool.Exec(ctx,
			`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
			memID, tenantID, userID)
		require.NoError(t, err)

		winnerID := uuid.New()
		_, err = rawPool.Exec(ctx, `
			INSERT INTO dept_memberships
				(id, tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by)
			VALUES ($1, $2, $3, $4, $5, 'preparator', $6)`,
			winnerID, tenantID, userID, memID, deptID, grantedBy)
		require.NoError(t, err)

		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewDeptMembershipRepository(appPool)

		out, prev, err := repo.Assign(tctx, tenantID, userID, deptID, memID, domain.DeptPreparator, grantedBy)
		require.NoError(t, err)
		require.NotNil(t, out)
		assert.Equal(t, domain.DeptPreparator, out.RoleLevel)
		assert.NotNil(t, prev, "must report the pre-existing row as previous")
		assert.Equal(t, winnerID, out.ID, "must return the pre-seeded winner row")
	})

	// DREPO-DM-006B — Assign level change winner SELECT fallback (lines 119,127,132,135)
	t.Run("Assign_LevelChange_WinnerFallback", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "dm-assign-winner-fallback")
		userID := uuid.New()
		deptID := uuid.New()
		grantedBy := uuid.New()

		_, err := rawPool.Exec(ctx,
			`INSERT INTO tenant_departments (tenant_id, department_id, is_active) VALUES ($1, $2, true)`,
			tenantID, deptID)
		require.NoError(t, err)

		memID := uuid.New()
		_, err = rawPool.Exec(ctx,
			`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
			memID, tenantID, userID)
		require.NoError(t, err)

		_, err = rawPool.Exec(ctx, `
			INSERT INTO dept_memberships
				(id, tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, 'preparator', $5)`,
			tenantID, userID, memID, deptID, grantedBy)
		require.NoError(t, err)

		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewDeptMembershipRepository(appPool)

		out, prev, err := repo.Assign(tctx, tenantID, userID, deptID, memID, domain.DeptApprover, grantedBy)
		require.NoError(t, err)
		require.NotNil(t, out)
		require.NotNil(t, prev)
		assert.Equal(t, domain.DeptApprover, out.RoleLevel)
		assert.Equal(t, domain.DeptPreparator, prev.RoleLevel)
	})

	// DREPO-DM-007 — SoftDeleteAllForDept ctx cancelled (lines 172,178)
	t.Run("SoftDeleteAllForDept_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "dm-softdeldept-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewDeptMembershipRepository(appPool)
		_, err := repo.SoftDeleteAllForDept(cancelCtx, tenantID, uuid.New())
		assert.Error(t, err, "cancelled context must return an error from SoftDeleteAllForDept")
	})

	// DREPO-DM-008 — SoftDeleteAllForUser ctx cancelled (lines 204,210)
	t.Run("SoftDeleteAllForUser_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "dm-softdeluser-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewDeptMembershipRepository(appPool)
		_, err := repo.SoftDeleteAllForUser(cancelCtx, tenantID, uuid.New())
		assert.Error(t, err, "cancelled context must return an error from SoftDeleteAllForUser")
	})

	// DREPO-DM-009 — Remove non-existent → ErrMemberNotFound (line 154)
	t.Run("Remove_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "dm-remove-notfound")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewDeptMembershipRepository(appPool)
		result, err := repo.Remove(tctx, tenantID, uuid.New(), uuid.New())
		assert.Nil(t, result)
		require.Error(t, err)
		var de *domain.DomainError
		require.ErrorAs(t, err, &de, "error must be a DomainError")
		assert.Equal(t, domain.ErrMemberNotFound.Error(), de.Code,
			"removing non-existent dept membership must yield ErrMemberNotFound")
	})
}

// ═════════════════════════════════════════════════════════════════════════════
// DeptRoleLabelRepository — error paths
// (one shared container, 5 subtests)
// ═════════════════════════════════════════════════════════════════════════════

func TestDRepo_DeptRoleLabel(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)

	// DREPO-DRL-001 — List ctx cancelled (lines 40,46)
	t.Run("List_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "drl-list-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewDeptRoleLabelRepository(appPool)
		_, err := repo.List(cancelCtx, tenantID)
		assert.Error(t, err, "cancelled context must return an error from List")
	})

	// DREPO-DRL-002 — Update no label row → ErrMemberNotFound (lines 69,70,73)
	t.Run("Update_NotFound_ErrMemberNotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "drl-update-notfound")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewDeptRoleLabelRepository(appPool)
		label, err := repo.Update(tctx, tenantID, domain.DeptPreparator, "Custom Preparator", 1)
		assert.Nil(t, label)
		require.Error(t, err)
		var de *domain.DomainError
		require.ErrorAs(t, err, &de, "error must be a DomainError")
		assert.Equal(t, domain.ErrMemberNotFound.Error(), de.Code,
			"missing label must yield ErrMemberNotFound")
	})

	// DREPO-DRL-003 — Update wrong version → ErrOptimisticLockConflict (lines 70,78)
	t.Run("Update_WrongVersion_OptimisticConflict", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "drl-update-conflict")

		_, err := rawPool.Exec(ctx, `
			INSERT INTO dept_role_labels (id, tenant_id, role_code, display_name)
			VALUES
				(gen_random_uuid(), $1, 'preparator', 'Preparator'),
				(gen_random_uuid(), $1, 'reviewer',   'Reviewer'),
				(gen_random_uuid(), $1, 'approver',   'Approver')
			ON CONFLICT (tenant_id, role_code) DO NOTHING`, tenantID)
		require.NoError(t, err)

		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewDeptRoleLabelRepository(appPool)

		label, err := repo.Update(tctx, tenantID, domain.DeptPreparator, "Overridden", 9999)
		assert.Nil(t, label)
		require.Error(t, err)
		var de *domain.DomainError
		require.ErrorAs(t, err, &de, "error must be a DomainError")
		assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code,
			"wrong record_version must yield ErrOptimisticLockConflict")
	})

	// DREPO-DRL-004 — Seed ctx cancelled (line 99)
	t.Run("Seed_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "drl-seed-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewDeptRoleLabelRepository(appPool)
		_, err := repo.Seed(cancelCtx, tenantID)
		assert.Error(t, err, "cancelled context must return an error from Seed")
	})

	// DREPO-DRL-005 — Update ctx cancelled (line 69)
	t.Run("Update_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "drl-update-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewDeptRoleLabelRepository(appPool)
		_, err := repo.Update(cancelCtx, tenantID, domain.DeptPreparator, "Whatever", 1)
		assert.Error(t, err, "cancelled context must return an error from Update")
	})
}

// ═════════════════════════════════════════════════════════════════════════════
// TenantRoleRepository — error paths
// (one shared container, 6 subtests)
// ═════════════════════════════════════════════════════════════════════════════

func TestDRepo_TenantRole(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)

	// DREPO-TR-001 — Grant duplicate is idempotent via ON CONFLICT (lines 94,107)
	t.Run("Grant_Idempotent_ConflictWinner", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tr-grant-idempotent")
		userID := uuid.New()
		grantedBy := uuid.New()

		memID := uuid.New()
		_, err := rawPool.Exec(ctx,
			`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
			memID, tenantID, userID)
		require.NoError(t, err)

		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewTenantRoleRepository(appPool)

		tr := &domain.TenantRole{
			TenantID:           tenantID,
			UserID:             userID,
			TenantMembershipID: memID,
			RoleCode:           domain.RoleTenantAdmin,
			GrantedBy:          grantedBy,
		}

		first, err := repo.Grant(tctx, tr)
		require.NoError(t, err)
		require.NotNil(t, first)
		assert.Equal(t, domain.RoleTenantAdmin, first.RoleCode)

		tr2 := &domain.TenantRole{
			TenantID:           tenantID,
			UserID:             userID,
			TenantMembershipID: memID,
			RoleCode:           domain.RoleTenantAdmin,
			GrantedBy:          grantedBy,
		}
		second, err := repo.Grant(tctx, tr2)
		require.NoError(t, err)
		require.NotNil(t, second)
		assert.Equal(t, first.ID, second.ID, "idempotent grant must return same row ID")
	})

	// DREPO-TR-002 — Grant ctx cancelled (line 94)
	t.Run("Grant_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tr-grant-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRoleRepository(appPool)
		tr := &domain.TenantRole{
			TenantID:           tenantID,
			UserID:             uuid.New(),
			TenantMembershipID: uuid.New(),
			RoleCode:           domain.RoleTenantAdmin,
			GrantedBy:          uuid.New(),
		}
		_, err := repo.Grant(cancelCtx, tr)
		assert.Error(t, err, "cancelled context must return an error from Grant")
	})

	// DREPO-TR-003 — Revoke non-existent role → ErrMemberNotFound (line 129)
	t.Run("Revoke_NotFound_ErrMemberNotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tr-revoke-notfound")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewTenantRoleRepository(appPool)
		result, err := repo.Revoke(tctx, tenantID, uuid.New(), domain.RoleTenantAdmin)
		assert.Nil(t, result)
		require.Error(t, err)
		var de *domain.DomainError
		require.ErrorAs(t, err, &de, "error must be a DomainError")
		assert.Equal(t, domain.ErrMemberNotFound.Error(), de.Code,
			"revoking a non-existent role must yield ErrMemberNotFound")
	})

	// DREPO-TR-004 — Revoke ctx cancelled (line 129)
	t.Run("Revoke_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tr-revoke-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRoleRepository(appPool)
		_, err := repo.Revoke(cancelCtx, tenantID, uuid.New(), domain.RoleTenantAdmin)
		assert.Error(t, err, "cancelled context must return an error from Revoke")
	})

	// DREPO-TR-005 — SoftDeleteAllForUser ctx cancelled (lines 145,151)
	t.Run("SoftDeleteAllForUser_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tr-softdeluser-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRoleRepository(appPool)
		_, err := repo.SoftDeleteAllForUser(cancelCtx, tenantID, uuid.New())
		assert.Error(t, err, "cancelled context must return an error from SoftDeleteAllForUser")
	})

	// DREPO-TR-006 — SoftDeleteAllForUser scan loop happy path (lines 145,151)
	t.Run("SoftDeleteAllForUser_ScanLoop", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tr-softdeluser-scan")
		userID := uuid.New()
		grantedBy := uuid.New()

		memID := uuid.New()
		_, err := rawPool.Exec(ctx,
			`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
			memID, tenantID, userID)
		require.NoError(t, err)

		_, err = rawPool.Exec(ctx, `
			INSERT INTO tenant_roles (id, tenant_id, user_id, tenant_membership_id, role_code, granted_by)
			VALUES (gen_random_uuid(), $1, $2, $3, 'tenant_admin', $4)`,
			tenantID, userID, memID, grantedBy)
		require.NoError(t, err)

		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewTenantRoleRepository(appPool)

		deleted, err := repo.SoftDeleteAllForUser(tctx, tenantID, userID)
		require.NoError(t, err)
		assert.Len(t, deleted, 1, "SoftDeleteAllForUser must return the soft-deleted role")
		assert.Equal(t, domain.RoleTenantAdmin, deleted[0].RoleCode)
	})
}
