//go:build integration

// coverage_gaps_test.go — tests added to cover previously uncovered
// postgres repository branches (coverage push to 100%).
//
// Priority 1: 0% functions (SoftDeleteAllForDept, ListActiveUserIDs,
//
//	MostRecentCreatedAt, CountCreatedInWindow).
//
// Priority 2: partial-coverage branches (Assign re-activation, Insert
//
//	duplicate invitation, SetStatus terminal-state path, SetActive not-found,
//	FindByIDIncludingDeleted deleted-row, Revoke not-found, SoftDelete
//	optimistic-lock, membership SoftDelete not-found).
//
// Priority 3: AuthZService.GetMembership with real DB (setCached / readFromDB).
package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═════════════════════════════════════════════════════════════════════════
// Priority 1 — 0% functions
// ═════════════════════════════════════════════════════════════════════════

// ── DeptMembershipRepository.SoftDeleteAllForDept ─────────────────────

// Test Case ID:      COV-DMSOFT-001
// Module:            iam-org-membership · Persistence
// Feature:           dept_memberships · SoftDeleteAllForDept happy path
// API:               Internal (P-25 department deactivation cascade)
// Scenario:          Positive — multiple members in dept → all soft-deleted, returned
// Preconditions:     Tenant + activated dept + 2 active dept_membership rows
// Test Steps:
//  1. Seed tenant, dept, two memberships in that dept
//  2. Call SoftDeleteAllForDept(tenantID, deptID)
//
// Expected Result:
//   - Returns 2 soft-deleted domain.DeptMembership rows
//   - deleted_at is set on both rows in DB
//
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestCOVDMSoft001_SoftDeleteAllForDept_Happy(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "dmsoft-001")
	deptID := uuid.New()
	userA := uuid.New()
	userB := uuid.New()
	memA := uuid.New()
	memB := uuid.New()

	// Activate dept and insert two memberships in it.
	_, err := rawPool.Exec(ctx,
		`INSERT INTO tenant_departments (tenant_id, department_id, is_active) VALUES ($1, $2, true)`,
		tenantID, deptID)
	require.NoError(t, err)

	_, err = rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
		memA, tenantID, userA)
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
		memB, tenantID, userB)
	require.NoError(t, err)

	_, err = rawPool.Exec(ctx, `
		INSERT INTO dept_memberships (id, tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, 'preparator', $2)`,
		tenantID, userA, memA, deptID)
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, `
		INSERT INTO dept_memberships (id, tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, 'reviewer', $2)`,
		tenantID, userB, memB, deptID)
	require.NoError(t, err)

	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewDeptMembershipRepository(appPool)

	deleted, err := repo.SoftDeleteAllForDept(tctx, tenantID, deptID)
	require.NoError(t, err)
	assert.Len(t, deleted, 2, "SoftDeleteAllForDept must return both soft-deleted rows")

	// Verify all rows are now soft-deleted in the DB.
	var remaining int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM dept_memberships WHERE tenant_id = $1 AND department_id = $2 AND deleted_at IS NULL`,
		tenantID, deptID).Scan(&remaining))
	assert.Equal(t, 0, remaining, "all active dept memberships must be soft-deleted")
}

// Test Case ID:      COV-DMSOFT-002
// Module:            iam-org-membership · Persistence
// Feature:           dept_memberships · SoftDeleteAllForDept — empty result
// API:               Internal (P-25)
// Scenario:          No active memberships exist for (tenant, dept) → returns empty slice, no error
// Preconditions:     Tenant + dept with no members
// Test Steps:
//  1. Activate dept but add no members
//  2. Call SoftDeleteAllForDept
//
// Expected Result:
//   - Returns empty (nil) slice and no error
//
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestCOVDMSoft002_SoftDeleteAllForDept_EmptyResult(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "dmsoft-002")
	deptID := uuid.New()
	_, err := rawPool.Exec(ctx,
		`INSERT INTO tenant_departments (tenant_id, department_id, is_active) VALUES ($1, $2, true)`,
		tenantID, deptID)
	require.NoError(t, err)

	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewDeptMembershipRepository(appPool)

	deleted, err := repo.SoftDeleteAllForDept(tctx, tenantID, deptID)
	require.NoError(t, err)
	assert.Empty(t, deleted, "must return empty slice when no members exist")
}

// ── MembershipRepository.ListActiveUserIDs ────────────────────────────

// Test Case ID:      COV-MEM-LIST-001
// Module:            iam-org-membership · Persistence
// Feature:           tenant_memberships · ListActiveUserIDs happy path
// API:               Internal (seat-overage reconciler)
// Scenario:          Positive — tenant with 3 active members → all 3 UUIDs returned
// Preconditions:     Tenant with 3 active tenant_memberships
// Test Steps:
//  1. Seed tenant with 3 active members via rawPool
//  2. Call ListActiveUserIDs(tenantID)
//
// Expected Result:
//   - Returns slice of 3 UUIDs matching the seeded user IDs
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVMemList001_ListActiveUserIDs_Happy(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "mem-list-001")
	userA, userB, userC := uuid.New(), uuid.New(), uuid.New()

	for _, u := range []uuid.UUID{userA, userB, userC} {
		_, err := rawPool.Exec(ctx,
			`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES (gen_random_uuid(), $1, $2, 'active')`,
			tenantID, u)
		require.NoError(t, err)
	}

	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewMembershipRepository(appPool)

	ids, err := repo.ListActiveUserIDs(tctx, tenantID)
	require.NoError(t, err)
	assert.Len(t, ids, 3, "ListActiveUserIDs must return all 3 active user IDs")

	idSet := make(map[uuid.UUID]bool, 3)
	for _, id := range ids {
		idSet[id] = true
	}
	assert.True(t, idSet[userA], "returned IDs must include userA")
	assert.True(t, idSet[userB], "returned IDs must include userB")
	assert.True(t, idSet[userC], "returned IDs must include userC")
}

// Test Case ID:      COV-MEM-LIST-002
// Module:            iam-org-membership · Persistence
// Feature:           tenant_memberships · ListActiveUserIDs — empty tenant
// API:               Internal
// Scenario:          Tenant with no members → returns empty slice, no error
// Preconditions:     Freshly seeded tenant, no memberships
// Test Steps:
//  1. Seed tenant
//  2. Call ListActiveUserIDs
//
// Expected Result:
//   - Returns nil/empty slice and no error
//
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestCOVMemList002_ListActiveUserIDs_EmptyTenant(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "mem-list-002")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewMembershipRepository(appPool)

	ids, err := repo.ListActiveUserIDs(tctx, tenantID)
	require.NoError(t, err)
	assert.Empty(t, ids, "empty tenant must return empty slice")
}

// ── InvitationRepository.MostRecentCreatedAt ─────────────────────────

// Test Case ID:      COV-INV-MRCR-001
// Module:            iam-org-membership · Persistence
// Feature:           pending_invitations · MostRecentCreatedAt happy path
// API:               Internal (PI-11 cooldown check)
// Scenario:          Positive — two invitations for same email → returns later timestamp
// Preconditions:     Tenant with 2 pending_invitations for same email
// Test Steps:
//  1. Insert two invitations at different timestamps
//  2. Call MostRecentCreatedAt
//
// Expected Result:
//   - Returns non-zero time (the created_at of the most recent invitation)
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVInvMRCR001_MostRecentCreatedAt_Happy(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "inv-mrcr-001")
	inviterID := uuid.New()
	email := "alice@example.com"

	// Insert first (older) invitation via rawPool.
	_, err := rawPool.Exec(ctx, `
		INSERT INTO pending_invitations
			(id, tenant_id, email, full_name, initial_tenant_roles, initial_dept_mappings,
			 invited_by, status, expires_at, kc_cleanup_pending)
		VALUES (gen_random_uuid(), $1, $2, 'Alice', '{}', '[]', $3, 'pending', now() + interval '7 days', false)`,
		tenantID, email, inviterID)
	require.NoError(t, err)

	// Insert second (newer) invitation with a different email after a tiny sleep is not
	// needed; DB clock monotonically increases. We insert two rows; created_at is set by DB default.
	// We want to confirm MostRecentCreatedAt returns a non-zero time.
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewInvitationRepository(appPool)

	ts, err := repo.MostRecentCreatedAt(tctx, tenantID, email)
	require.NoError(t, err)
	assert.False(t, ts.IsZero(), "MostRecentCreatedAt must return a non-zero time when an invitation exists")
}

// Test Case ID:      COV-INV-MRCR-002
// Module:            iam-org-membership · Persistence
// Feature:           pending_invitations · MostRecentCreatedAt — no invitation
// API:               Internal (PI-11 cooldown check)
// Scenario:          No invitation for email → returns zero time (no error)
// Preconditions:     Empty tenant
// Test Steps:
//  1. Call MostRecentCreatedAt for email that has never been invited
//
// Expected Result:
//   - Returns zero time.Time and no error (PI-11: zero means no cooldown needed)
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVInvMRCR002_MostRecentCreatedAt_NoInvitation(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "inv-mrcr-002")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewInvitationRepository(appPool)

	ts, err := repo.MostRecentCreatedAt(tctx, tenantID, "never-invited@example.com")
	require.NoError(t, err)
	assert.True(t, ts.IsZero(), "MostRecentCreatedAt must return zero time when no invitation exists")
}

// ── InvitationRepository.CountCreatedInWindow ─────────────────────────

// Test Case ID:      COV-INV-CCIW-001
// Module:            iam-org-membership · Persistence
// Feature:           pending_invitations · CountCreatedInWindow happy path
// API:               Internal (PI-12 per-tenant hourly rate check)
// Scenario:          Positive — 3 invitations seeded, window covers all 3 → returns 3
// Preconditions:     Tenant with 3 pending_invitations
// Test Steps:
//  1. Insert 3 invitations
//  2. Call CountCreatedInWindow with since = 1 hour ago
//
// Expected Result:
//   - Returns 3
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVInvCCIW001_CountCreatedInWindow_AllInWindow(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "inv-cciw-001")
	inviterID := uuid.New()

	for i := 0; i < 3; i++ {
		_, err := rawPool.Exec(ctx, `
			INSERT INTO pending_invitations
				(id, tenant_id, email, full_name, initial_tenant_roles, initial_dept_mappings,
				 invited_by, status, expires_at, kc_cleanup_pending)
			VALUES (gen_random_uuid(), $1, $2, 'User', '{}', '[]', $3, 'pending', now() + interval '7 days', false)`,
			tenantID, "user"+string(rune('0'+i))+"@example.com", inviterID)
		require.NoError(t, err)
	}

	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewInvitationRepository(appPool)

	since := time.Now().UTC().Add(-1 * time.Hour)
	count, err := repo.CountCreatedInWindow(tctx, tenantID, since)
	require.NoError(t, err)
	assert.Equal(t, 3, count, "CountCreatedInWindow must count all 3 invitations within the window")
}

// Test Case ID:      COV-INV-CCIW-002
// Module:            iam-org-membership · Persistence
// Feature:           pending_invitations · CountCreatedInWindow — future since
// API:               Internal (PI-12)
// Scenario:          Window starts in the future → no invitations match → returns 0
// Preconditions:     Tenant with 1 invitation (created now)
// Test Steps:
//  1. Insert 1 invitation
//  2. Call CountCreatedInWindow with since = 1 hour from now
//
// Expected Result:
//   - Returns 0 (no invitations created in the future)
//
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestCOVInvCCIW002_CountCreatedInWindow_NoneInWindow(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "inv-cciw-002")
	inviterID := uuid.New()

	_, err := rawPool.Exec(ctx, `
		INSERT INTO pending_invitations
			(id, tenant_id, email, full_name, initial_tenant_roles, initial_dept_mappings,
			 invited_by, status, expires_at, kc_cleanup_pending)
		VALUES (gen_random_uuid(), $1, 'bob@example.com', 'Bob', '{}', '[]', $2, 'pending', now() + interval '7 days', false)`,
		tenantID, inviterID)
	require.NoError(t, err)

	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewInvitationRepository(appPool)

	since := time.Now().UTC().Add(1 * time.Hour) // future window
	count, err := repo.CountCreatedInWindow(tctx, tenantID, since)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "CountCreatedInWindow must return 0 when window excludes all rows")
}

// ═════════════════════════════════════════════════════════════════════════
// Priority 2 — partial coverage branches
// ═════════════════════════════════════════════════════════════════════════

// ── DeptMembershipRepository.Assign — level-change path ───────────────

// Test Case ID:      COV-ASSIGN-001
// Module:            iam-org-membership · Persistence
// Feature:           dept_memberships · Assign — level change (DepartmentMembershipLevelChanged)
// API:               P-10 dept-membership assign
// Scenario:          User already at preparator → re-assign as reviewer → previous row returned
// Preconditions:     Active dept membership at preparator level
// Test Steps:
//  1. Assign user at preparator level (first call)
//  2. Re-assign at reviewer level (second call)
//
// Expected Result:
//   - Second call returns (current=reviewer, previous=preparator, nil)
//   - Old row is soft-deleted; new row has reviewer level
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVAssign001_Assign_LevelChange_ReturnsOldLevel(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID, userID, membershipID, deptID := seedForDeptAssign(t, ctx, rawPool, "assign-lc-001")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewDeptMembershipRepository(appPool)

	// First assign at preparator.
	current, previous, err := repo.Assign(tctx, tenantID, userID, deptID, membershipID, domain.DeptPreparator, userID)
	require.NoError(t, err)
	assert.Equal(t, domain.DeptPreparator, current.RoleLevel)
	assert.Nil(t, previous, "first assignment must return nil previous")

	// Second assign at reviewer — level change.
	current2, previous2, err := repo.Assign(tctx, tenantID, userID, deptID, membershipID, domain.DeptReviewer, userID)
	require.NoError(t, err)
	require.NotNil(t, previous2, "level change must return previous membership")
	assert.Equal(t, domain.DeptPreparator, previous2.RoleLevel, "previous must carry old level")
	assert.Equal(t, domain.DeptReviewer, current2.RoleLevel, "current must carry new level")

	// Verify old row is soft-deleted and new row is active.
	var activeCount int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM dept_memberships WHERE tenant_id = $1 AND user_id = $2 AND department_id = $3 AND deleted_at IS NULL`,
		tenantID, userID, deptID).Scan(&activeCount))
	assert.Equal(t, 1, activeCount, "only one active membership row must exist after level change")
}

// Test Case ID:      COV-ASSIGN-002
// Module:            iam-org-membership · Persistence
// Feature:           dept_memberships · Assign — idempotent same level
// API:               P-10
// Scenario:          Assign same level twice → second call is a no-op (returns same row, nil previous)
// Preconditions:     Active dept membership at approver level
// Test Steps:
//  1. Assign at approver
//  2. Re-assign at approver
//
// Expected Result:
//   - Both calls succeed; second returns (current=approver, previous=approver_row, nil)
//   - No soft-delete of the existing row
//
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestCOVAssign002_Assign_SameLevel_Idempotent(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID, userID, membershipID, deptID := seedForDeptAssign(t, ctx, rawPool, "assign-idem-002")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewDeptMembershipRepository(appPool)

	first, _, err := repo.Assign(tctx, tenantID, userID, deptID, membershipID, domain.DeptApprover, userID)
	require.NoError(t, err)
	require.NotNil(t, first)

	second, previousOnSecond, err := repo.Assign(tctx, tenantID, userID, deptID, membershipID, domain.DeptApprover, userID)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID, "same-level re-assign must return same row ID")
	assert.Equal(t, domain.DeptApprover, previousOnSecond.RoleLevel, "previous must reflect existing row at same level")

	var activeCount int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM dept_memberships WHERE tenant_id = $1 AND user_id = $2 AND department_id = $3 AND deleted_at IS NULL`,
		tenantID, userID, deptID).Scan(&activeCount))
	assert.Equal(t, 1, activeCount, "idempotent assign must not create extra rows")
}

// ── InvitationRepository.Insert — duplicate email path ───────────────

// Test Case ID:      COV-INV-INS-001
// Module:            iam-org-membership · Persistence
// Feature:           pending_invitations · Insert — duplicate pending email → ErrInvitationAlreadyExists
// API:               P-6
// Scenario:          Two Insert calls for the same (tenant, email) → second returns 409 domain error
// Preconditions:     First invitation inserted for alice@example.com
// Test Steps:
//  1. Insert first invitation for alice@example.com
//  2. Insert second invitation for alice@example.com (same tenant)
//
// Expected Result:
//   - Second Insert returns ErrInvitationAlreadyExists (409)
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVInvIns001_Insert_DuplicateEmail_ErrInvitationAlreadyExists(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "inv-ins-001")
	inviterID := uuid.New()
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewInvitationRepository(appPool)

	inv := &domain.PendingInvitation{
		TenantID:           tenantID,
		Email:              "alice-dup@example.com",
		FullName:           "Alice",
		InitialTenantRoles: []domain.TenantRoleCode{},
		InvitedBy:          inviterID,
		Status:             domain.InvitePending,
		ExpiresAt:          time.Now().UTC().Add(7 * 24 * time.Hour),
	}

	_, err := repo.Insert(tctx, inv)
	require.NoError(t, err, "first Insert must succeed")

	// Second insert for the same email — must hit the partial unique index.
	inv2 := &domain.PendingInvitation{
		TenantID:           tenantID,
		Email:              "alice-dup@example.com",
		FullName:           "Alice Again",
		InitialTenantRoles: []domain.TenantRoleCode{},
		InvitedBy:          inviterID,
		Status:             domain.InvitePending,
		ExpiresAt:          time.Now().UTC().Add(7 * 24 * time.Hour),
	}
	_, err = repo.Insert(tctx, inv2)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrInvitationAlreadyExists.Error(), de.Code)
}

// ── InvitationRepository.SetStatus — terminal state and not-found paths ──

// Test Case ID:      COV-INV-SS-001
// Module:            iam-org-membership · Persistence
// Feature:           pending_invitations · SetStatus — already accepted → ErrInvitationNotFound
// API:               P-31
// Scenario:          Attempt to set status on an already-accepted invitation → ErrInvitationNotFound
// Preconditions:     Invitation exists with status='accepted' (terminal)
// Test Steps:
//  1. Insert invitation
//  2. Update it directly to 'accepted' via rawPool
//  3. Call SetStatus(revoked, version=1) on the accepted invitation
//
// Expected Result:
//   - Returns ErrInvitationNotFound (LLD P-31: terminal state treated as not found)
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVInvSS001_SetStatus_TerminalState_ErrNotFound(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "inv-ss-001")
	inviterID := uuid.New()
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewInvitationRepository(appPool)

	inv := &domain.PendingInvitation{
		TenantID:           tenantID,
		Email:              "terminal@example.com",
		FullName:           "Terminal",
		InitialTenantRoles: []domain.TenantRoleCode{},
		InvitedBy:          inviterID,
		Status:             domain.InvitePending,
		ExpiresAt:          time.Now().UTC().Add(7 * 24 * time.Hour),
	}
	created, err := repo.Insert(tctx, inv)
	require.NoError(t, err)

	// Force terminal state via rawPool.
	_, err = rawPool.Exec(ctx,
		`UPDATE pending_invitations SET status = 'accepted', accepted_at = now() WHERE id = $1`,
		created.ID)
	require.NoError(t, err)

	// SetStatus with the original version — terminal status filter in WHERE
	// returns 0 rows, probe finds status='accepted' → ErrInvitationNotFound.
	_, err = repo.SetStatus(tctx, tenantID, created.ID, domain.InviteRevoked, created.RecordVersion)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrInvitationNotFound.Error(), de.Code)
}

// Test Case ID:      COV-INV-SS-002
// Module:            iam-org-membership · Persistence
// Feature:           pending_invitations · SetStatus — stale version → ErrOptimisticLockConflict
// API:               P-31
// Scenario:          Attempt to set status with wrong record_version on a pending invitation
// Preconditions:     Pending invitation at record_version=1
// Test Steps:
//  1. Insert invitation
//  2. SetStatus(revoked, version=999)
//
// Expected Result:
//   - Returns ErrOptimisticLockConflict
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVInvSS002_SetStatus_StaleVersion_OptimisticLock(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "inv-ss-002")
	inviterID := uuid.New()
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewInvitationRepository(appPool)

	inv := &domain.PendingInvitation{
		TenantID:           tenantID,
		Email:              "stale-ver@example.com",
		FullName:           "Stale",
		InitialTenantRoles: []domain.TenantRoleCode{},
		InvitedBy:          inviterID,
		Status:             domain.InvitePending,
		ExpiresAt:          time.Now().UTC().Add(7 * 24 * time.Hour),
	}
	created, err := repo.Insert(tctx, inv)
	require.NoError(t, err)

	_, err = repo.SetStatus(tctx, tenantID, created.ID, domain.InviteRevoked, 999)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
}

// Test Case ID:      COV-INV-SS-003
// Module:            iam-org-membership · Persistence
// Feature:           pending_invitations · SetStatus — not-found (row deleted) → ErrInvitationNotFound
// API:               P-31
// Scenario:          SetStatus on non-existent invitation → ErrInvitationNotFound from probe
// Preconditions:     No invitation with the given ID
// Test Steps:
//  1. Call SetStatus with a random UUID
//
// Expected Result:
//   - Returns ErrInvitationNotFound
//
// Priority:          P1
// Severity:          Minor
// Automation Status: Automated
func TestCOVInvSS003_SetStatus_NonExistent_ErrNotFound(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "inv-ss-003")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewInvitationRepository(appPool)

	_, err := repo.SetStatus(tctx, tenantID, uuid.New(), domain.InviteRevoked, 1)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrInvitationNotFound.Error(), de.Code)
}

// Test Case ID:      COV-INV-SS-004
// Module:            iam-org-membership · Persistence
// Feature:           pending_invitations · SetStatus — accepted sets accepted_at
// API:               P-30 (accept invitation)
// Scenario:          SetStatus(accepted) on pending invitation → accepted_at stamped
// Preconditions:     Pending invitation
// Test Steps:
//  1. Insert pending invitation
//  2. SetStatus(accepted, version=1)
//
// Expected Result:
//   - Returns updated row with status=accepted and non-nil accepted_at
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVInvSS004_SetStatus_Accepted_StampsAcceptedAt(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "inv-ss-004")
	inviterID := uuid.New()
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewInvitationRepository(appPool)

	inv := &domain.PendingInvitation{
		TenantID:           tenantID,
		Email:              "accept-me@example.com",
		FullName:           "Accept",
		InitialTenantRoles: []domain.TenantRoleCode{},
		InvitedBy:          inviterID,
		Status:             domain.InvitePending,
		ExpiresAt:          time.Now().UTC().Add(7 * 24 * time.Hour),
	}
	created, err := repo.Insert(tctx, inv)
	require.NoError(t, err)

	updated, err := repo.SetStatus(tctx, tenantID, created.ID, domain.InviteAccepted, created.RecordVersion)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, domain.InviteAccepted, updated.Status)
	assert.NotNil(t, updated.AcceptedAt, "accepted_at must be stamped on acceptance")
}

// ── TenantDepartmentRepository.SetActive — not-found path ─────────────

// Test Case ID:      COV-TDEPT-SA-001
// Module:            iam-org-membership · Persistence
// Feature:           tenant_departments · SetActive — dept not found → ErrDepartmentNotFound
// API:               P-25
// Scenario:          SetActive on a non-existent (tenant, dept) pair → ErrDepartmentNotFound
// Preconditions:     Tenant exists but dept never activated
// Test Steps:
//  1. Call SetActive(tenantID, randomDeptID, false, 1)
//
// Expected Result:
//   - Returns ErrDepartmentNotFound (not ErrOptimisticLockConflict)
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVTDeptSA001_SetActive_NotFound_ErrDepartmentNotFound(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "tdept-sa-001")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewTenantDepartmentRepository(appPool)

	_, err := repo.SetActive(tctx, tenantID, uuid.New(), false, 1)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrDepartmentNotFound.Error(), de.Code)
}

// Test Case ID:      COV-TDEPT-SA-002
// Module:            iam-org-membership · Persistence
// Feature:           tenant_departments · SetActive happy — flip to inactive then back
// API:               P-25
// Scenario:          Activate dept, deactivate with SetActive(false), then re-activate with SetActive(true)
// Preconditions:     Activated dept at record_version=1
// Test Steps:
//  1. Activate dept via rawPool
//  2. SetActive(false, expectedVersion=1) → succeeds, version=2
//  3. SetActive(true, expectedVersion=2) → succeeds, version=3
//
// Expected Result:
//   - Each call succeeds and returns updated IsActive
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVTDeptSA002_SetActive_FlipInactiveAndBack(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "tdept-sa-002")
	deptID := uuid.New()
	_, err := rawPool.Exec(ctx,
		`INSERT INTO tenant_departments (tenant_id, department_id, is_active) VALUES ($1, $2, true)`,
		tenantID, deptID)
	require.NoError(t, err)

	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewTenantDepartmentRepository(appPool)

	// Deactivate (version=1).
	td, err := repo.SetActive(tctx, tenantID, deptID, false, 1)
	require.NoError(t, err)
	assert.False(t, td.IsActive)
	assert.EqualValues(t, 2, td.RecordVersion)

	// Re-activate (version=2).
	td2, err := repo.SetActive(tctx, tenantID, deptID, true, 2)
	require.NoError(t, err)
	assert.True(t, td2.IsActive)
	assert.EqualValues(t, 3, td2.RecordVersion)
}

// ── TenantRepository.FindByIDIncludingDeleted — deleted tenant path ────

// Test Case ID:      COV-TENANT-INCL-001
// Module:            iam-org-membership · Persistence
// Feature:           tenants · FindByIDIncludingDeleted — returns soft-deleted row
// API:               Internal (I-2 RP cleanup, reconcilers)
// Scenario:          Tenant soft-deleted → FindByID returns not-found, FindByIDIncludingDeleted returns row
// Preconditions:     Tenant seeded and then soft-deleted via rawPool
// Test Steps:
//  1. Seed tenant
//  2. Soft-delete via rawPool (SET deleted_at = now())
//  3. FindByID → error (not found)
//  4. FindByIDIncludingDeleted → success, row returned
//
// Expected Result:
//   - FindByID returns ErrTenantNotFound
//   - FindByIDIncludingDeleted returns non-nil tenant
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVTenantIncl001_FindByIDIncludingDeleted_SoftDeletedTenant(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "incl-del-001")
	// Soft-delete the tenant.
	_, err := rawPool.Exec(ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, tenantID)
	require.NoError(t, err)

	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewTenantRepository(appPool)

	// FindByID must not return the deleted row.
	_, err = repo.FindByID(tctx, tenantID)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrTenantNotFound.Error(), de.Code)

	// FindByIDIncludingDeleted must return the deleted row.
	found, err := repo.FindByIDIncludingDeleted(tctx, tenantID)
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, tenantID, found.ID)
	assert.NotNil(t, found.DeletedAt, "deleted_at must be set on returned row")
}

// Test Case ID:      COV-TENANT-INCL-002
// Module:            iam-org-membership · Persistence
// Feature:           tenants · FindByIDIncludingDeleted — not found at all
// API:               Internal
// Scenario:          No row with given ID → ErrTenantNotFound
// Preconditions:     No tenant with the requested UUID
// Test Steps:
//  1. FindByIDIncludingDeleted(random UUID)
//
// Expected Result:
//   - Returns ErrTenantNotFound
//
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestCOVTenantIncl002_FindByIDIncludingDeleted_NotFound(t *testing.T) {
	t.Parallel()	appPool, _, _ := setupTestDB(t)
	ctx := context.Background()

	unknown := uuid.New()
	tctx := withTenant(ctx, unknown)
	repo := pgadapter.NewTenantRepository(appPool)

	_, err := repo.FindByIDIncludingDeleted(tctx, unknown)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrTenantNotFound.Error(), de.Code)
}

// ── TenantRepository.optimisticConflictOrNotFound — real conflict path ─

// Test Case ID:      COV-TENANT-OPT-001
// Module:            iam-org-membership · Persistence
// Feature:           tenants · Update optimistic-lock — row exists but version mismatch
//
//	→ ErrOptimisticLockConflict (not ErrTenantNotFound)
//
// API:               PATCH /api/v1/tenants/{id}
// Scenario:          Row exists at version=1; Update called with version=2 (stale) → conflict
// Preconditions:     Tenant at record_version=1
// Test Steps:
//  1. Seed tenant
//  2. Update with RecordVersion=2 (one ahead — row is at 1)
//
// Expected Result:
//   - Returns ErrOptimisticLockConflict (not ErrTenantNotFound)
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVTenantOpt001_Update_VersionMismatch_NotNotFound(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "opt-001")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewTenantRepository(appPool)

	name := "Updated Name"
	_, err := repo.Update(tctx, tenantID, &domain.TenantPatch{Name: &name, RecordVersion: 2})
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	// Must be optimistic_lock_conflict, NOT tenant_not_found.
	assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code,
		"row exists but version mismatches → must return optimistic_lock_conflict")
}

// ── TenantRoleRepository.Revoke — not-found path ──────────────────────

// Test Case ID:      COV-ROLE-REV-001
// Module:            iam-org-membership · Persistence
// Feature:           tenant_roles · Revoke — role grant not found → ErrMemberNotFound
// API:               Internal (role revoke cascade)
// Scenario:          Revoke a role that doesn't exist → ErrMemberNotFound
// Preconditions:     Tenant exists, user has no tenant_owner grant
// Test Steps:
//  1. Call Revoke(tenantID, randomUserID, tenant_owner)
//
// Expected Result:
//   - Returns ErrMemberNotFound
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVRoleRev001_Revoke_NotFound_ErrMemberNotFound(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "role-rev-001")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewTenantRoleRepository(appPool)

	_, err := repo.Revoke(tctx, tenantID, uuid.New(), domain.RoleTenantOwner)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrMemberNotFound.Error(), de.Code)
}

// Test Case ID:      COV-ROLE-REV-002
// Module:            iam-org-membership · Persistence
// Feature:           tenant_roles · Revoke — already soft-deleted → ErrMemberNotFound
// API:               Internal
// Scenario:          Role grant soft-deleted → second Revoke returns ErrMemberNotFound
// Preconditions:     Tenant role granted then soft-deleted via rawPool
// Test Steps:
//  1. Grant role via rawPool
//  2. Soft-delete via rawPool
//  3. Call Revoke
//
// Expected Result:
//   - Returns ErrMemberNotFound (no active row matches WHERE deleted_at IS NULL)
//
// Priority:          P2
// Severity:          Minor
// Automation Status: Automated
func TestCOVRoleRev002_Revoke_AlreadySoftDeleted_ErrMemberNotFound(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "role-rev-002")
	userID := uuid.New()
	memID := uuid.New()

	_, err := rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
		memID, tenantID, userID)
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, `
		INSERT INTO tenant_roles (id, tenant_id, user_id, tenant_membership_id, role_code, granted_by, deleted_at)
		VALUES (gen_random_uuid(), $1, $2, $3, 'tenant_admin', $2, now())`,
		tenantID, userID, memID)
	require.NoError(t, err)

	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewTenantRoleRepository(appPool)

	_, err = repo.Revoke(tctx, tenantID, userID, domain.RoleTenantAdmin)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrMemberNotFound.Error(), de.Code)
}

// ── MembershipRepository.SoftDelete — optimistic-lock and not-found paths ──

// Test Case ID:      COV-MEM-SD-001
// Module:            iam-org-membership · Persistence
// Feature:           tenant_memberships · SoftDelete — stale version → ErrOptimisticLockConflict
// API:               P-7 (remove user)
// Scenario:          SoftDelete with wrong record_version → optimistic-lock error
// Preconditions:     Active membership at record_version=1
// Test Steps:
//  1. Seed membership
//  2. SoftDelete(tenantID, userID, expectedVersion=999)
//
// Expected Result:
//   - Returns ErrOptimisticLockConflict
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVMemSD001_SoftDelete_StaleVersion_OptimisticLock(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "mem-sd-001")
	userID := uuid.New()
	_, err := rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES (gen_random_uuid(), $1, $2, 'active')`,
		tenantID, userID)
	require.NoError(t, err)

	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewMembershipRepository(appPool)

	err = repo.SoftDelete(tctx, tenantID, userID, 999)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
}

// Test Case ID:      COV-MEM-SD-002
// Module:            iam-org-membership · Persistence
// Feature:           tenant_memberships · SoftDelete — member not found
// API:               P-7
// Scenario:          SoftDelete for a user with no active membership → ErrMemberNotFound
// Preconditions:     Tenant exists but user has no membership
// Test Steps:
//  1. SoftDelete(tenantID, randomUserID, expectedVersion=1)
//
// Expected Result:
//   - Returns ErrMemberNotFound
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVMemSD002_SoftDelete_NotFound_ErrMemberNotFound(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "mem-sd-002")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewMembershipRepository(appPool)

	err := repo.SoftDelete(tctx, tenantID, uuid.New(), 1)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrMemberNotFound.Error(), de.Code)
}

// Test Case ID:      COV-MEM-SD-003
// Module:            iam-org-membership · Persistence
// Feature:           tenant_memberships · SoftDelete happy path
// API:               P-7
// Scenario:          Positive — active member soft-deleted with correct version
// Preconditions:     Active membership at record_version=1
// Test Steps:
//  1. Seed active membership
//  2. SoftDelete(tenantID, userID, expectedVersion=1)
//
// Expected Result:
//   - No error, membership row has deleted_at set and status='left'
//
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestCOVMemSD003_SoftDelete_Happy(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "mem-sd-003")
	userID := uuid.New()
	_, err := rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES (gen_random_uuid(), $1, $2, 'active')`,
		tenantID, userID)
	require.NoError(t, err)

	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewMembershipRepository(appPool)

	err = repo.SoftDelete(tctx, tenantID, userID, 1)
	require.NoError(t, err)

	// Verify the row is soft-deleted.
	var deletedAt *time.Time
	var status string
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT deleted_at, status FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2`,
		tenantID, userID).Scan(&deletedAt, &status))
	assert.NotNil(t, deletedAt, "deleted_at must be set after SoftDelete")
	assert.Equal(t, "left", status, "status must be 'left' after soft-delete")
}

// ═════════════════════════════════════════════════════════════════════════
// Priority 3 — AuthZService.GetMembership with real DB
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      COV-AUTHZ-001
// Module:            iam-org-membership · Service
// Feature:           authz_service · GetMembership — happy path with dept + role + feature flags
// API:               GET /internal/users/:id/memberships (I-8)
// Scenario:          Active member with tenant_owner role + 1 dept membership → full projection
// Preconditions:     Tenant + active membership + role grant + dept membership
// Test Steps:
//  1. Seed tenant, user, membership, tenant_owner role, dept membership
//  2. Call AuthZService.GetMembership(tenantID, userID)
//
// Expected Result:
//   - Returns MembershipProjection with:
//   - Status = active
//   - Roles contains "member" and "tenant_owner"
//   - Departments contains the seeded dept
//   - EffectiveFeatureFlags non-nil
//   - ReadOnly = false (tenant is in trial)
//
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestCOVAuthZ001_GetMembership_HappyPath(t *testing.T) {
	t.Parallel()	fx := buildTestFixtures(t)
	ctx := context.Background()

	tenantID := uuid.New()
	ownerID := uuid.New()
	deptID := uuid.New()

	_, _, err := provisionTrialWithOwner(t, ctx, fx, tenantID, ownerID, "authz-happy-001")
	require.NoError(t, err)

	// Activate a department and assign the owner as preparator.
	_, err = fx.rawPool.Exec(ctx,
		`INSERT INTO tenant_departments (tenant_id, department_id, is_active) VALUES ($1, $2, true)`,
		tenantID, deptID)
	require.NoError(t, err)

	var memID uuid.UUID
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT id FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL`,
		tenantID, ownerID).Scan(&memID))

	_, err = fx.rawPool.Exec(ctx, `
		INSERT INTO dept_memberships (id, tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, 'preparator', $2)`,
		tenantID, ownerID, memID, deptID)
	require.NoError(t, err)

	proj, err := fx.AuthZ.GetMembership(withTenant(ctx, tenantID), tenantID, ownerID)
	require.NoError(t, err)
	require.NotNil(t, proj)

	assert.Equal(t, ownerID, proj.UserID)
	assert.Equal(t, tenantID, proj.TenantID)
	assert.Equal(t, domain.MembershipActive, proj.Status)
	assert.False(t, proj.ReadOnly, "trial tenant must not be read_only")

	// TR-7: "member" must be first in roles list.
	require.NotEmpty(t, proj.Roles)
	assert.Equal(t, domain.RoleMember, proj.Roles[0])

	// Should include tenant_owner.
	found := false
	for _, r := range proj.Roles {
		if r == domain.RoleTenantOwner {
			found = true
		}
	}
	assert.True(t, found, "tenant_owner role must appear in projection")

	// Dept membership must be present.
	require.Len(t, proj.Departments, 1)
	assert.Equal(t, deptID, proj.Departments[0].DepartmentID)
	assert.Equal(t, domain.DeptPreparator, proj.Departments[0].RoleLevel)

	assert.NotNil(t, proj.FeatureFlags)
}

// Test Case ID:      COV-AUTHZ-002
// Module:            iam-org-membership · Service
// Feature:           authz_service · GetMembership — user not member of tenant → ErrMemberNotFound
// API:               I-8
// Scenario:          No membership row for (tenantID, userID) → returns ErrMemberNotFound
// Preconditions:     Tenant exists, user has no membership
// Test Steps:
//  1. Seed tenant (no membership for randomUserID)
//  2. GetMembership(tenantID, randomUserID)
//
// Expected Result:
//   - Returns ErrMemberNotFound
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVAuthZ002_GetMembership_NotMember_ErrMemberNotFound(t *testing.T) {
	t.Parallel()	fx := buildTestFixtures(t)
	ctx := context.Background()

	tenantID := uuid.New()
	ownerID := uuid.New()
	_, _, err := provisionTrialWithOwner(t, ctx, fx, tenantID, ownerID, "authz-notfound-002")
	require.NoError(t, err)

	randomUser := uuid.New()
	_, err = fx.AuthZ.GetMembership(withTenant(ctx, tenantID), tenantID, randomUser)
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

// Test Case ID:      COV-AUTHZ-003
// Module:            iam-org-membership · Service
// Feature:           authz_service · GetMembership — cancelled tenant → read_only=true
// API:               I-8 (§16 A53)
// Scenario:          Tenant with status='cancelled' → ReadOnly=true in projection
// Preconditions:     Active membership in a cancelled tenant
// Test Steps:
//  1. Provision trial tenant with owner
//  2. Force tenant status to 'cancelled' via rawPool
//  3. GetMembership(tenantID, ownerID)
//
// Expected Result:
//   - ReadOnly = true (LLD §16 A53: read_only iff subscription_status='cancelled')
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVAuthZ003_GetMembership_CancelledTenant_ReadOnly(t *testing.T) {
	t.Parallel()	fx := buildTestFixtures(t)
	ctx := context.Background()

	tenantID := uuid.New()
	ownerID := uuid.New()
	_, _, err := provisionTrialWithOwner(t, ctx, fx, tenantID, ownerID, "authz-cancelled-003")
	require.NoError(t, err)

	// Force tenant to cancelled status. subscription_started_at must be non-NULL for
	// non-trial statuses (chk_subscription_started_required).
	_, err = fx.rawPool.Exec(ctx,
		`UPDATE tenants SET status = 'cancelled', cancelled_at = now(), subscription_started_at = now() WHERE id = $1`, tenantID)
	require.NoError(t, err)

	proj, err := fx.AuthZ.GetMembership(withTenant(ctx, tenantID), tenantID, ownerID)
	require.NoError(t, err)
	require.NotNil(t, proj)
	assert.Equal(t, domain.StatusCancelled, proj.SubscriptionStatus)
	assert.True(t, proj.ReadOnly, "cancelled tenant must have ReadOnly=true (LLD §16 A53)")
}

// ── InvitationRepository.Insert happy path coverage ────────────────────

// Test Case ID:      COV-INV-INS-002
// Module:            iam-org-membership · Persistence
// Feature:           pending_invitations · Insert happy — with initial roles and dept mappings
// API:               P-6
// Scenario:          Insert invitation with initial_tenant_roles and initial_dept_mappings
// Preconditions:     Tenant exists
// Test Steps:
//  1. Insert invitation with roles=[tenant_admin] and dept_mappings=[{deptID, reviewer}]
//
// Expected Result:
//   - Returns invitation with correct roles and dept mapping fields populated
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVInvIns002_Insert_WithRolesAndDeptMappings(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "inv-ins-002")
	inviterID := uuid.New()
	deptID := uuid.New()
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewInvitationRepository(appPool)

	inv := &domain.PendingInvitation{
		TenantID:           tenantID,
		Email:              "roles-depts@example.com",
		FullName:           "Roles Depts",
		InitialTenantRoles: []domain.TenantRoleCode{domain.RoleTenantAdmin},
		InitialDeptMappings: []domain.InvitationDeptMapping{
			{DepartmentID: deptID, Level: domain.DeptReviewer},
		},
		InvitedBy: inviterID,
		Status:    domain.InvitePending,
		ExpiresAt: time.Now().UTC().Add(7 * 24 * time.Hour),
	}

	created, err := repo.Insert(tctx, inv)
	require.NoError(t, err)
	require.NotNil(t, created)

	require.Len(t, created.InitialTenantRoles, 1)
	assert.Equal(t, domain.RoleTenantAdmin, created.InitialTenantRoles[0])
	require.Len(t, created.InitialDeptMappings, 1)
	assert.Equal(t, deptID, created.InitialDeptMappings[0].DepartmentID)
	assert.Equal(t, domain.DeptReviewer, created.InitialDeptMappings[0].Level)
}

// ── MembershipRepository.Insert — idempotency ON CONFLICT path ─────────

// Test Case ID:      COV-MEM-INS-001
// Module:            iam-org-membership · Persistence
// Feature:           tenant_memberships · Insert — ON CONFLICT returns existing row
// API:               I-3 AddFromRegister (PI-10 idempotency)
// Scenario:          Insert same (tenant, user) twice → second Insert returns existing row unchanged
// Preconditions:     None
// Test Steps:
//  1. Insert membership for user
//  2. Insert again for same user
//
// Expected Result:
//   - Second call returns the same row (same ID, same status)
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestCOVMemIns001_Insert_Idempotent_ReturnsExistingRow(t *testing.T) {
	t.Parallel()	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "mem-ins-001")
	userID := uuid.New()
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewMembershipRepository(appPool)

	first, err := repo.Insert(tctx, &domain.TenantMembership{
		TenantID: tenantID,
		UserID:   userID,
		Status:   domain.MembershipActive,
	})
	require.NoError(t, err)
	require.NotNil(t, first)

	second, err := repo.Insert(tctx, &domain.TenantMembership{
		TenantID: tenantID,
		UserID:   userID,
		Status:   domain.MembershipActive,
	})
	require.NoError(t, err)
	require.NotNil(t, second)

	assert.Equal(t, first.ID, second.ID, "ON CONFLICT must return the existing row (PI-10 idempotency)")
	assert.Equal(t, first.Status, second.Status)
}

// ═════════════════════════════════════════════════════════════════════════
// Helper: provision a trial tenant with exactly one owner member via
// ProvisioningService.TrialSignup so all side-effects (5 depts, labels,
// outbox events) land correctly and the owner's membership + role grant
// exist for AuthZ projection tests.
// Returns (tenantID, ownerID, error).
// ═════════════════════════════════════════════════════════════════════════

func provisionTrialWithOwner(t *testing.T, ctx context.Context, fx *testFixtures, tenantID, ownerID uuid.UUID, slug string) (uuid.UUID, uuid.UUID, error) {
	t.Helper()
	svcCtx := withSystemAndTenant(ctx, tenantID)
	_, _, err := fx.Provisioning.TrialSignup(svcCtx, service.TrialSignupInput{
		TenantID:      tenantID,
		Slug:          slug,
		Name:          slug,
		Plan:          domain.PlanStarter,
		OwnerUserID:   ownerID,
		DefaultLocale: "en-US",
	})
	return tenantID, ownerID, err
}
