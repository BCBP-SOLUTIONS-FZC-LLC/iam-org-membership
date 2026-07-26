//go:build integration

// Regression tests for repository/DB-level audit fixes (Tier 1-3 + Round 2).
// Uses the same testcontainers setup as rls_test.go (setupTestDB, seedTenant,
// withTenant). Each test names the fix ID it locks in; a green run means
// PI-10 idempotency, CONC-1 optimistic locking, and the trigger/CHECK
// invariants are all intact.
package postgres_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────
// B3 + B14: MembershipRepository.Insert
//   - PI-10 idempotency: second Insert returns the existing row, no error.
//   - B14 lifecycle-laundering guard: a `suspended` row is NOT reactivated
//     back to `active` by a repeat Insert.
// ─────────────────────────────────────────────────────────────────────────

func TestB3_MembershipInsert_Idempotent(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "acme-b3")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewMembershipRepository(appPool)

	userID := uuid.New()
	first, err := repo.Insert(tctx, &domain.TenantMembership{
		TenantID: tenantID, UserID: userID, Status: domain.MembershipActive,
	})
	require.NoError(t, err)
	require.NotNil(t, first)

	// Second Insert with same (tenant_id, user_id) — must NOT error, must
	// return the existing row (PI-10).
	second, err := repo.Insert(tctx, &domain.TenantMembership{
		TenantID: tenantID, UserID: userID, Status: domain.MembershipActive,
	})
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, first.ID, second.ID, "PI-10: same row returned")
	assert.Equal(t, first.RecordVersion, second.RecordVersion,
		"touch_row no-op suppression: version unchanged on identical write")
}

func TestB14_MembershipInsert_PreservesSuspendedStatus(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "acme-b14")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewMembershipRepository(appPool)

	userID := uuid.New()
	created, err := repo.Insert(tctx, &domain.TenantMembership{
		TenantID: tenantID, UserID: userID, Status: domain.MembershipActive,
	})
	require.NoError(t, err)

	// Directly flip to suspended (simulating I-4 lifecycle patch).
	suspended, err := repo.SetStatus(tctx, tenantID, userID, "suspended", created.RecordVersion)
	require.NoError(t, err)
	assert.Equal(t, domain.MembershipStatus("suspended"), suspended.Status)

	// Now a KC re-register replay calls Insert again with status='active'.
	// B14 requires: the row STAYS suspended. No silent lifecycle laundering.
	replayed, err := repo.Insert(tctx, &domain.TenantMembership{
		TenantID: tenantID, UserID: userID, Status: domain.MembershipActive,
	})
	require.NoError(t, err)
	assert.Equal(t, domain.MembershipStatus("suspended"), replayed.Status,
		"B14: suspended row must not be laundered to active by re-Insert")
	assert.Equal(t, suspended.RecordVersion, replayed.RecordVersion,
		"no version bump when Insert absorbed by ON CONFLICT DO NOTHING")
}

// ─────────────────────────────────────────────────────────────────────────
// B4: DeptMembershipRepository.Assign
//   - Same-level replay short-circuits without touching the row.
//   - Concurrent grants of the same key resolve without raw unique-violation.
//   - Level change soft-deletes old + inserts new.
// ─────────────────────────────────────────────────────────────────────────

func TestB4_DeptMembershipAssign_SameLevelNoOp(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID, userID, membershipID, deptID := seedForDeptAssign(t, ctx, rawPool, "acme-b4a")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewDeptMembershipRepository(appPool)

	first, err := repo.Assign(tctx, tenantID, userID, deptID, membershipID, domain.DeptReviewer, uuid.Nil)
	require.NoError(t, err)

	second, err := repo.Assign(tctx, tenantID, userID, deptID, membershipID, domain.DeptReviewer, uuid.Nil)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID, "same-level replay must return existing row")
	assert.Equal(t, first.RecordVersion, second.RecordVersion, "no version bump on no-op")
}

func TestB4_DeptMembershipAssign_LevelChange(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID, userID, membershipID, deptID := seedForDeptAssign(t, ctx, rawPool, "acme-b4b")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewDeptMembershipRepository(appPool)

	first, err := repo.Assign(tctx, tenantID, userID, deptID, membershipID, domain.DeptPreparator, uuid.Nil)
	require.NoError(t, err)

	// Level change → old soft-deleted, new inserted.
	second, err := repo.Assign(tctx, tenantID, userID, deptID, membershipID, domain.DeptApprover, uuid.Nil)
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, second.ID, "level change gets a fresh row for audit history")
	assert.Equal(t, domain.DeptApprover, second.RoleLevel)
}

func TestB4_DeptMembershipAssign_ConcurrentCreates_OneWinsGracefully(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID, userID, membershipID, deptID := seedForDeptAssign(t, ctx, rawPool, "acme-b4c")
	tctx := withTenant(ctx, tenantID)
	repo := pgadapter.NewDeptMembershipRepository(appPool)

	// Race two Assigns for the same (tenant, user, dept). Only one should
	// win the INSERT; the other must be absorbed by ON CONFLICT DO NOTHING
	// and return the winner via the fallback SELECT. Neither should error.
	var wg sync.WaitGroup
	results := make([]*domain.DeptMembership, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Small stagger so both goroutines have a real chance to overlap.
			time.Sleep(time.Duration(idx) * 5 * time.Millisecond)
			out, err := repo.Assign(tctx, tenantID, userID, deptID, membershipID, domain.DeptReviewer, uuid.Nil)
			results[idx] = out
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.NotNil(t, results[0])
	require.NotNil(t, results[1])
	assert.Equal(t, results[0].ID, results[1].ID,
		"both goroutines must resolve to the same row (winner returned to both)")
}

// ─────────────────────────────────────────────────────────────────────────
// B2: TenantRoleRepository.Grant is ON CONFLICT DO NOTHING + fallback SELECT.
// ─────────────────────────────────────────────────────────────────────────

func TestB2_TenantRoleGrant_Idempotent(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "acme-b2")
	tctx := withTenant(ctx, tenantID)

	memRepo := pgadapter.NewMembershipRepository(appPool)
	userID := uuid.New()
	mem, err := memRepo.Insert(tctx, &domain.TenantMembership{
		TenantID: tenantID, UserID: userID, Status: domain.MembershipActive,
	})
	require.NoError(t, err)

	roles := pgadapter.NewTenantRoleRepository(appPool)
	first, err := roles.Grant(tctx, &domain.TenantRole{
		TenantID: tenantID, UserID: userID, TenantMembershipID: mem.ID,
		RoleCode: domain.RoleTenderAdmin, GrantedBy: uuid.Nil,
	})
	require.NoError(t, err)

	// Second grant — LLD §5.4 O-7 line 2069 mandates ON CONFLICT DO NOTHING.
	second, err := roles.Grant(tctx, &domain.TenantRole{
		TenantID: tenantID, UserID: userID, TenantMembershipID: mem.ID,
		RoleCode: domain.RoleTenderAdmin, GrantedBy: uuid.Nil,
	})
	require.NoError(t, err, "B2: replay must be idempotent, not raw 23505")
	assert.Equal(t, first.ID, second.ID)
}

// ─────────────────────────────────────────────────────────────────────────
// G5: BEFORE INSERT trigger blocks pending_invitations with expires_at <= now().
// ─────────────────────────────────────────────────────────────────────────

func TestG5_PendingInvitationExpiryGuard_BlocksPastTimestamp(t *testing.T) {
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "acme-g5")

	// Attempt to insert a row whose expires_at is already in the past.
	// PI-1: must be rejected by the trigger with check_violation.
	_, err := rawPool.Exec(ctx, `
		INSERT INTO pending_invitations
		  (id, tenant_id, email, full_name, invited_by, status, expires_at)
		VALUES
		  ($1, $2, 'stale@example.com', 'Stale', $3, 'pending', now() - interval '1 minute')`,
		uuid.New(), tenantID, uuid.New())
	require.Error(t, err, "G5: past expires_at must be rejected at insert time")
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	// The trigger raises with ERRCODE = 'check_violation' which is the
	// SQL standard code 23514 (not the generic PL/pgSQL P0001).
	assert.Equal(t, "23514", pgErr.Code, "check_violation SQLSTATE per LLD §16 A11")
}

func TestG5_PendingInvitationExpiryGuard_AllowsFutureTimestamp(t *testing.T) {
	_, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "acme-g5b")

	_, err := rawPool.Exec(ctx, `
		INSERT INTO pending_invitations
		  (id, tenant_id, email, full_name, invited_by, status, expires_at)
		VALUES
		  ($1, $2, 'fresh@example.com', 'Fresh', $3, 'pending', now() + interval '7 days')`,
		uuid.New(), tenantID, uuid.New())
	require.NoError(t, err, "future expires_at is allowed")
}

// ─────────────────────────────────────────────────────────────────────────
// B16: tenants RLS policy routes through rls_check_tenant() so cross-tenant
// reads/writes fire iam_rls_violations_total (via log_rls_violation()).
// ─────────────────────────────────────────────────────────────────────────

func TestB16_TenantsRLSPolicy_UsesHelperFunction(t *testing.T) {
	_, rawPool := setupTestDB(t)
	ctx := context.Background()

	// Confirm the tenants policy's USING expression references
	// rls_check_tenant(...) — the audit-logging helper. After migration
	// 000008, both USING and WITH CHECK must invoke it.
	var qual, withCheck string
	err := rawPool.QueryRow(ctx, `
		SELECT qual, with_check
		FROM pg_policies
		WHERE schemaname = 'public' AND tablename = 'tenants'
		  AND policyname = 'tenant_isolation'`).Scan(&qual, &withCheck)
	require.NoError(t, err)
	assert.Contains(t, qual, "rls_check_tenant",
		"B16: tenants USING clause must use rls_check_tenant helper")
	assert.Contains(t, withCheck, "rls_check_tenant",
		"B16: tenants WITH CHECK clause must use rls_check_tenant helper")
}

// ─────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────

// seedForDeptAssign creates tenant + tenant_departments row + membership
// so DeptMembership.Assign can succeed (its precondition is an active
// tenant_departments row).
func seedForDeptAssign(t *testing.T, ctx context.Context, rawPool *pgxpool.Pool, slug string) (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	tenantID := seedTenant(t, ctx, rawPool, slug)
	userID := uuid.New()
	deptID := uuid.New()
	membershipID := uuid.New()

	_, err := rawPool.Exec(ctx, `INSERT INTO departments (id, code, name, is_system) VALUES ($1, $2, $3, false)`,
		deptID, "TEST_"+strings.ToUpper(slug), "Test "+slug)
	require.NoError(t, err)

	_, err = rawPool.Exec(ctx, `INSERT INTO tenant_departments (tenant_id, department_id, is_active) VALUES ($1, $2, true)`,
		tenantID, deptID)
	require.NoError(t, err)

	_, err = rawPool.Exec(ctx, `INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
		membershipID, tenantID, userID)
	require.NoError(t, err)

	return tenantID, userID, membershipID, deptID
}

