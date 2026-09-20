//go:build integration

// repo_error_paths_test.go — error-path coverage for postgres repository functions.
//
// Covers previously uncovered branches across:
//   - invitation_repository.go   lines: 48,52,62,68,82,83,86,103,123,140,142,
//     152,160,186,199,209,240,249,262,272,293,299,313,319,372
//   - membership_repository.go   lines: 64,70,77,102,132,148,170,184,210,216,233
//   - tenant_repository.go       lines: 51,73,90,114,133,151,154,169,173,209,
//     241,247,254,263,286,315,329,351,356,386,409,424,428,476,479,500
//   - reconciler_store.go        lines: 34,40,58,64,89,109,128
//   - idempotency_repository.go  line:  37
//   - authz_repository.go        lines: 56,64,70,77,86,92,100,106,124
//
// Tests are grouped into 6 parent functions, each sharing ONE testcontainer.
// Subtests run in parallel within each group. This keeps wall-clock time
// manageable on CI runners without sacrificing coverage or data isolation
// (every subtest seeds its own tenant with a unique slug and UUID).
//
// Techniques used:
//  1. Context cancellation   → triggers query/exec errors on any DB call.
//  2. Non-existent UUIDs     → ErrNoRows / not-found paths.
//  3. Nil inputs             → validation-error paths (patch==nil, t==nil).
//  4. Wrong record_version   → optimistic-lock conflict (RowsAffected==0).
//  5. Unknown lifecycle op   → default branch in execLifecyclePatch.
package postgres_test

import (
	"context"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═══════════════════════════════════════════════════════════════════════════
// InvitationRepository — error paths  (one shared container, 25 subtests)
// ═══════════════════════════════════════════════════════════════════════════

func TestErrPaths_InvitationRepo(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)

	// ERRPATH-INV-001 — List ctx cancelled (lines 62,68)
	t.Run("List_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-list-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err := repo.List(cancelCtx, tenantID)
		assert.Error(t, err, "cancelled context must return an error")
	})

	// ERRPATH-INV-002 — LockByID not found → nil, nil (lines 82,83,86)
	t.Run("LockByID_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-lock-notfound")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewInvitationRepository(appPool)
		inv, err := repo.LockByID(tctx, uuid.New())
		require.NoError(t, err, "LockByID with non-existent ID must not error")
		assert.Nil(t, inv, "LockByID with non-existent ID must return nil invitation")
	})

	// ERRPATH-INV-003 — LockByID ctx cancelled (line 82)
	t.Run("LockByID_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-lock-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err := repo.LockByID(cancelCtx, uuid.New())
		assert.Error(t, err)
	})

	// ERRPATH-INV-004 — FindByID not found → ErrInvitationNotFound (lines 100,101,103)
	t.Run("FindByID_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-findbyid-notfound")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewInvitationRepository(appPool)
		inv, err := repo.FindByID(tctx, tenantID, uuid.New())
		assert.Nil(t, inv)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrInvitationNotFound.Error(), de.Code)
	})

	// ERRPATH-INV-005 — FindByID ctx cancelled (line 98)
	t.Run("FindByID_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-findbyid-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err := repo.FindByID(cancelCtx, tenantID, uuid.New())
		assert.Error(t, err)
	})

	// ERRPATH-INV-006 — FindPendingByEmail ctx cancelled (lines 117,123)
	t.Run("FindPendingByEmail_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-email-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err := repo.FindPendingByEmail(cancelCtx, tenantID, "test@example.com")
		assert.Error(t, err)
	})

	// ERRPATH-INV-007 — FindPendingByKeycloakUser not found → nil, nil (lines 136,140,142)
	t.Run("FindPendingByKeycloakUser_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-kc-notfound")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewInvitationRepository(appPool)
		inv, err := repo.FindPendingByKeycloakUser(tctx, tenantID, uuid.New())
		require.NoError(t, err)
		assert.Nil(t, inv)
	})

	// ERRPATH-INV-008 — FindPendingByKeycloakUser ctx cancelled (line 134)
	t.Run("FindPendingByKeycloakUser_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-kc-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err := repo.FindPendingByKeycloakUser(cancelCtx, tenantID, uuid.New())
		assert.Error(t, err)
	})

	// ERRPATH-INV-009 — Insert ctx cancelled (lines 168,186)
	t.Run("Insert_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-insert-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		inv := &domain.PendingInvitation{
			TenantID:  tenantID,
			Email:     "test@example.com",
			FullName:  "Test User",
			InvitedBy: uuid.New(),
			ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
		}
		_, err := repo.Insert(cancelCtx, inv)
		assert.Error(t, err)
	})

	// ERRPATH-INV-010 — SetKeycloakUserID not found (lines 199,209)
	t.Run("SetKeycloakUserID_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-setkc-notfound")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewInvitationRepository(appPool)
		err := repo.SetKeycloakUserID(tctx, tenantID, uuid.New(), uuid.New(), 1)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrInvitationNotFound.Error(), de.Code)
	})

	// ERRPATH-INV-011 — SetKeycloakUserID optimistic conflict (lines 199,211)
	t.Run("SetKeycloakUserID_OptimisticConflict", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-setkc-conflict")
		tctx := withTenant(ctx, tenantID)
		invID := uuid.New()
		_, err := rawPool.Exec(ctx, `
			INSERT INTO pending_invitations (id, tenant_id, email, full_name, initial_tenant_roles,
				initial_dept_mappings, invited_by, status, expires_at, kc_cleanup_pending)
			VALUES ($1, $2, 'conflict@example.com', 'Conflict User', '{}', '[]',
				$3, 'pending', now() + interval '7 days', false)`,
			invID, tenantID, uuid.New())
		require.NoError(t, err)
		repo := pgadapter.NewInvitationRepository(appPool)
		err = repo.SetKeycloakUserID(tctx, tenantID, invID, uuid.New(), 9999) // wrong version
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
	})

	// ERRPATH-INV-012 — SetKeycloakUserID ctx cancelled (line 196)
	t.Run("SetKeycloakUserID_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-setkc-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		err := repo.SetKeycloakUserID(cancelCtx, tenantID, uuid.New(), uuid.New(), 1)
		assert.Error(t, err)
	})

	// ERRPATH-INV-013 — SetStatus not found (lines 232,237,238,240)
	t.Run("SetStatus_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-setstatus-notfound")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err := repo.SetStatus(tctx, tenantID, uuid.New(), domain.InviteAccepted, 1)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrInvitationNotFound.Error(), de.Code)
	})

	// ERRPATH-INV-014 — SetStatus terminal state → ErrInvitationNotFound (lines 240-249)
	t.Run("SetStatus_TerminalState", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-setstatus-terminal")
		tctx := withTenant(ctx, tenantID)
		invID := uuid.New()
		_, err := rawPool.Exec(ctx, `
			INSERT INTO pending_invitations (id, tenant_id, email, full_name, initial_tenant_roles,
				initial_dept_mappings, invited_by, status, expires_at, kc_cleanup_pending)
			VALUES ($1, $2, 'terminal@example.com', 'Terminal User', '{}', '[]',
				$3, 'pending', now() + interval '7 days', false)`,
			invID, tenantID, uuid.New())
		require.NoError(t, err)
		_, err = rawPool.Exec(ctx,
			`UPDATE pending_invitations SET status = 'expired' WHERE id = $1`, invID)
		require.NoError(t, err)
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err = repo.SetStatus(tctx, tenantID, invID, domain.InviteAccepted, 1)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrInvitationNotFound.Error(), de.Code)
	})

	// ERRPATH-INV-015 — SetStatus optimistic conflict (lines 232,246,247)
	t.Run("SetStatus_OptimisticConflict", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-setstatus-conflict")
		tctx := withTenant(ctx, tenantID)
		invID := uuid.New()
		_, err := rawPool.Exec(ctx, `
			INSERT INTO pending_invitations (id, tenant_id, email, full_name, initial_tenant_roles,
				initial_dept_mappings, invited_by, status, expires_at, kc_cleanup_pending)
			VALUES ($1, $2, 'pending-conflict@example.com', 'Pending User', '{}', '[]',
				$3, 'pending', now() + interval '7 days', false)`,
			invID, tenantID, uuid.New())
		require.NoError(t, err)
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err = repo.SetStatus(tctx, tenantID, invID, domain.InviteAccepted, 9999) // wrong version
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
	})

	// ERRPATH-INV-016 — SetStatus ctx cancelled (line 225)
	t.Run("SetStatus_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-setstatus-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err := repo.SetStatus(cancelCtx, tenantID, uuid.New(), domain.InviteAccepted, 1)
		assert.Error(t, err)
	})

	// ERRPATH-INV-017 — SetKCCleanupPending not found (lines 262,269,272)
	t.Run("SetKCCleanupPending_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-kcleanup-notfound")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewInvitationRepository(appPool)
		err := repo.SetKCCleanupPending(tctx, tenantID, uuid.New(), true, 1)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrInvitationNotFound.Error(), de.Code)
	})

	// ERRPATH-INV-018 — SetKCCleanupPending optimistic conflict (lines 262,274)
	t.Run("SetKCCleanupPending_OptimisticConflict", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-kcleanup-conflict")
		tctx := withTenant(ctx, tenantID)
		invID := uuid.New()
		_, err := rawPool.Exec(ctx, `
			INSERT INTO pending_invitations (id, tenant_id, email, full_name, initial_tenant_roles,
				initial_dept_mappings, invited_by, status, expires_at, kc_cleanup_pending)
			VALUES ($1, $2, 'kcleanup@example.com', 'KC Cleanup User', '{}', '[]',
				$3, 'pending', now() + interval '7 days', false)`,
			invID, tenantID, uuid.New())
		require.NoError(t, err)
		repo := pgadapter.NewInvitationRepository(appPool)
		err = repo.SetKCCleanupPending(tctx, tenantID, invID, true, 9999) // wrong version
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
	})

	// ERRPATH-INV-019 — SetKCCleanupPending ctx cancelled (line 259)
	t.Run("SetKCCleanupPending_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-kcleanup-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		err := repo.SetKCCleanupPending(cancelCtx, tenantID, uuid.New(), true, 1)
		assert.Error(t, err)
	})

	// ERRPATH-INV-020 — ListExpiring ctx cancelled (lines 293,299)
	t.Run("ListExpiring_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-listexpiring-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err := repo.ListExpiring(cancelCtx, time.Now().Add(24*time.Hour), 10)
		assert.Error(t, err)
	})

	// ERRPATH-INV-021 — ListPendingKCCleanup ctx cancelled (lines 313,319)
	t.Run("ListPendingKCCleanup_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-pendingkc-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err := repo.ListPendingKCCleanup(cancelCtx, 10)
		assert.Error(t, err)
	})

	// ERRPATH-INV-022 — ExpireOverdue ctx cancelled (line 372)
	t.Run("ExpireOverdue_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-expire-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err := repo.ExpireOverdue(cancelCtx, 100)
		assert.Error(t, err)
	})

	// ERRPATH-INV-023 — MostRecentCreatedAt ctx cancelled (line 333)
	t.Run("MostRecentCreatedAt_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-mostrecent-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err := repo.MostRecentCreatedAt(cancelCtx, tenantID, "test@example.com")
		assert.Error(t, err)
	})

	// ERRPATH-INV-024 — CountCreatedInWindow ctx cancelled (line 351)
	t.Run("CountCreatedInWindow_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-countwindow-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err := repo.CountCreatedInWindow(cancelCtx, tenantID, time.Now().Add(-time.Hour))
		assert.Error(t, err)
	})

	// ERRPATH-INV-025 — CountPending ctx cancelled (line 283)
	t.Run("CountPending_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "inv-countpending-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewInvitationRepository(appPool)
		_, err := repo.CountPending(cancelCtx, tenantID)
		assert.Error(t, err)
	})
}

// ═══════════════════════════════════════════════════════════════════════════
// MembershipRepository — error paths  (one shared container, 12 subtests)
// ═══════════════════════════════════════════════════════════════════════════

func TestErrPaths_MembershipRepo(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)

	// ERRPATH-MEM-001 — List ctx cancelled no cursor (lines 64,77)
	t.Run("List_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "mem-list-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewMembershipRepository(appPool)
		_, err := repo.List(cancelCtx, tenantID, nil, 10)
		assert.Error(t, err)
	})

	// ERRPATH-MEM-002 — List ctx cancelled with cursor (lines 64,77)
	t.Run("List_WithCursor_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "mem-list-cursor-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewMembershipRepository(appPool)
		cursor := &domain.MembershipListCursor{CreatedAt: time.Now(), ID: uuid.New()}
		_, err := repo.List(cancelCtx, tenantID, cursor, 10)
		assert.Error(t, err)
	})

	// ERRPATH-MEM-003 — FindByUserID not found → ErrMemberNotFound (lines 99,100,102)
	t.Run("FindByUserID_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "mem-findbyuser-notfound")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewMembershipRepository(appPool)
		m, err := repo.FindByUserID(tctx, tenantID, uuid.New())
		assert.Nil(t, m)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrMemberNotFound.Error(), de.Code)
	})

	// ERRPATH-MEM-004 — FindByUserID ctx cancelled (line 96)
	t.Run("FindByUserID_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "mem-findbyuser-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewMembershipRepository(appPool)
		_, err := repo.FindByUserID(cancelCtx, tenantID, uuid.New())
		assert.Error(t, err)
	})

	// ERRPATH-MEM-005 — Insert ctx cancelled (lines 124,132)
	t.Run("Insert_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "mem-insert-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewMembershipRepository(appPool)
		tm := &domain.TenantMembership{
			TenantID: tenantID,
			UserID:   uuid.New(),
			Status:   domain.MembershipActive,
		}
		_, err := repo.Insert(cancelCtx, tm)
		assert.Error(t, err)
	})

	// ERRPATH-MEM-006 — SetStatus not found (lines 167,168,170)
	t.Run("SetStatus_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "mem-setstatus-notfound")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewMembershipRepository(appPool)
		_, err := repo.SetStatus(tctx, tenantID, uuid.New(), domain.MembershipSuspended, 1)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrMemberNotFound.Error(), de.Code)
	})

	// ERRPATH-MEM-007 — SetStatus optimistic conflict (lines 167,168,184)
	t.Run("SetStatus_OptimisticConflict", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "mem-setstatus-conflict")
		userID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		_, err := rawPool.Exec(ctx,
			`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES (gen_random_uuid(), $1, $2, 'active')`,
			tenantID, userID)
		require.NoError(t, err)
		repo := pgadapter.NewMembershipRepository(appPool)
		_, err = repo.SetStatus(tctx, tenantID, userID, domain.MembershipSuspended, 9999)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
	})

	// ERRPATH-MEM-008 — SetStatus ctx cancelled (line 160)
	t.Run("SetStatus_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "mem-setstatus-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewMembershipRepository(appPool)
		_, err := repo.SetStatus(cancelCtx, tenantID, uuid.New(), domain.MembershipSuspended, 1)
		assert.Error(t, err)
	})

	// ERRPATH-MEM-009 — SoftDelete not found (lines 184,187,188,210,216,233)
	t.Run("SoftDelete_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "mem-softdel-notfound")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewMembershipRepository(appPool)
		err := repo.SoftDelete(tctx, tenantID, uuid.New(), 1)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrMemberNotFound.Error(), de.Code)
	})

	// ERRPATH-MEM-010 — SoftDelete optimistic conflict (lines 184,187,188,233)
	t.Run("SoftDelete_OptimisticConflict", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "mem-softdel-conflict")
		userID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		_, err := rawPool.Exec(ctx,
			`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES (gen_random_uuid(), $1, $2, 'active')`,
			tenantID, userID)
		require.NoError(t, err)
		repo := pgadapter.NewMembershipRepository(appPool)
		err = repo.SoftDelete(tctx, tenantID, userID, 9999)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
	})

	// ERRPATH-MEM-011 — SoftDelete ctx cancelled (line 180)
	t.Run("SoftDelete_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "mem-softdel-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewMembershipRepository(appPool)
		err := repo.SoftDelete(cancelCtx, tenantID, uuid.New(), 1)
		assert.Error(t, err)
	})

	// ERRPATH-MEM-012 — ListActiveUserIDs ctx cancelled (line 210)
	t.Run("ListActiveUserIDs_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "mem-listuids-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewMembershipRepository(appPool)
		_, err := repo.ListActiveUserIDs(cancelCtx, tenantID)
		assert.Error(t, err)
	})
}

// ═══════════════════════════════════════════════════════════════════════════
// TenantRepository — error paths  (one shared container, 34 subtests)
// ═══════════════════════════════════════════════════════════════════════════

func TestErrPaths_TenantRepo(t *testing.T) {
	t.Parallel()
	appPool, rawPool, sysPool := setupTestDB(t)

	// ERRPATH-TENANT-001 — FindByID not found (lines 47,48,51)
	t.Run("FindByID_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		seedTenant(t, ctx, rawPool, "tenant-findbyid-notfound")
		tctx := withTenant(ctx, uuid.New()) // GUC to unknown tenant
		repo := pgadapter.NewTenantRepository(appPool)
		ten, err := repo.FindByID(tctx, uuid.New())
		assert.Nil(t, ten)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrTenantNotFound.Error(), de.Code)
	})

	// ERRPATH-TENANT-002 — FindByID ctx cancelled (line 47)
	t.Run("FindByID_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-findbyid-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		_, err := repo.FindByID(cancelCtx, tenantID)
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-003 — FindByIDIncludingDeleted not found (lines 70,71,73)
	t.Run("FindByIDIncludingDeleted_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		repo := pgadapter.NewTenantRepository(sysPool)
		ten, err := repo.FindByIDIncludingDeleted(ctx, uuid.New())
		assert.Nil(t, ten)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrTenantNotFound.Error(), de.Code)
	})

	// ERRPATH-TENANT-004 — FindByIDIncludingDeleted ctx cancelled (line 67)
	t.Run("FindByIDIncludingDeleted_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		repo := pgadapter.NewTenantRepository(sysPool)
		_, err := repo.FindByIDIncludingDeleted(cancelCtx, uuid.New())
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-005 — Update nil patch → ErrValidation (lines 90,91)
	t.Run("Update_NilPatch", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-update-nilpatch")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewTenantRepository(appPool)
		ten, err := repo.Update(tctx, tenantID, nil)
		assert.Nil(t, ten)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrValidation.Error(), de.Code)
	})

	// ERRPATH-TENANT-006 — Update empty patch → returns current row (lines 114,115,116)
	t.Run("Update_EmptyPatch", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-update-emptypatch")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewTenantRepository(appPool)
		patch := &domain.TenantPatch{RecordVersion: 1}
		ten, err := repo.Update(tctx, tenantID, patch)
		require.NoError(t, err)
		assert.NotNil(t, ten, "empty patch must return current tenant row")
	})

	// ERRPATH-TENANT-007 — Update wrong version → ErrOptimisticLockConflict (lines 127,128,133)
	t.Run("Update_OptimisticConflict", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-update-conflict")
		tctx := withTenant(ctx, tenantID)
		name := "Updated Name"
		repo := pgadapter.NewTenantRepository(appPool)
		patch := &domain.TenantPatch{RecordVersion: 9999, Name: &name}
		_, err := repo.Update(tctx, tenantID, patch)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
	})

	// ERRPATH-TENANT-008 — Update non-existent tenant → ErrTenantNotFound
	t.Run("Update_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		nonExistentID := uuid.New()
		tctx := withTenant(ctx, nonExistentID)
		seedTenant(t, ctx, rawPool, "tenant-update-notfound")
		name := "Ghost Tenant"
		repo := pgadapter.NewTenantRepository(appPool)
		patch := &domain.TenantPatch{RecordVersion: 1, Name: &name}
		_, err := repo.Update(tctx, nonExistentID, patch)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrTenantNotFound.Error(), de.Code)
	})

	// ERRPATH-TENANT-009 — SetRealmSyncPending not found (lines 151,154)
	t.Run("SetRealmSyncPending_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		nonExistentID := uuid.New()
		tctx := withTenant(ctx, nonExistentID)
		seedTenant(t, ctx, rawPool, "tenant-realmsyncp-notfound")
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.SetRealmSyncPending(tctx, nonExistentID)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrTenantNotFound.Error(), de.Code)
	})

	// ERRPATH-TENANT-010 — SetRealmSyncPending ctx cancelled (line 148)
	t.Run("SetRealmSyncPending_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-realmsyncp-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.SetRealmSyncPending(cancelCtx, tenantID)
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-011 — Insert nil tenant → ErrValidation (lines 169,173)
	t.Run("Insert_NilTenant", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		repo := pgadapter.NewTenantRepository(appPool)
		out, wasCreated, err := repo.Insert(ctx, nil)
		assert.Nil(t, out)
		assert.False(t, wasCreated)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrValidation.Error(), de.Code)
	})

	// ERRPATH-TENANT-012 — Insert ctx cancelled (line 185)
	t.Run("Insert_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		t2 := &domain.Tenant{
			Slug:      "insert-ctx-cancelled",
			Name:      "Test Tenant",
			Plan:      domain.PlanStarter,
			Status:    domain.StatusTrial,
			RealmID:   "realm-test",
			RealmType: domain.RealmShared,
		}
		_, _, err := repo.Insert(cancelCtx, t2)
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-013 — ListSubscriptionLapses ctx cancelled (lines 233,241,247,254)
	t.Run("ListSubscriptionLapses_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		repo := pgadapter.NewTenantRepository(sysPool)
		_, err := repo.ListSubscriptionLapses(cancelCtx, 30)
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-014 — LockByID not found (lines 263,266,267)
	t.Run("LockByID_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		nonExistentID := uuid.New()
		tctx := withTenant(ctx, nonExistentID)
		seedTenant(t, ctx, rawPool, "tenant-lockbyid-notfound")
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.LockByID(tctx, nonExistentID)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrTenantNotFound.Error(), de.Code)
	})

	// ERRPATH-TENANT-015 — LockByID ctx cancelled (line 262)
	t.Run("LockByID_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-lockbyid-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.LockByID(cancelCtx, tenantID)
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-016 — SetFeatureFlags not found (lines 286,293,295)
	t.Run("SetFeatureFlags_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		nonExistentID := uuid.New()
		tctx := withTenant(ctx, nonExistentID)
		seedTenant(t, ctx, rawPool, "tenant-setflags-notfound")
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.SetFeatureFlags(tctx, nonExistentID, []byte(`{}`), 1)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrTenantNotFound.Error(), de.Code)
	})

	// ERRPATH-TENANT-017 — SetFeatureFlags optimistic conflict (lines 286,297,298)
	t.Run("SetFeatureFlags_OptimisticConflict", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-setflags-conflict")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.SetFeatureFlags(tctx, tenantID, []byte(`{"feature_x": true}`), 9999)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
	})

	// ERRPATH-TENANT-018 — SetFeatureFlags ctx cancelled (line 283)
	t.Run("SetFeatureFlags_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-setflags-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.SetFeatureFlags(cancelCtx, tenantID, []byte(`{}`), 1)
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-019 — MarkOwnerlessIfUnset ctx cancelled (line 314)
	t.Run("MarkOwnerlessIfUnset_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-ownerless-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		_, err := repo.MarkOwnerlessIfUnset(cancelCtx, tenantID)
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-020 — SetRealmFields not found (lines 329,333,336)
	t.Run("SetRealmFields_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		nonExistentID := uuid.New()
		tctx := withTenant(ctx, nonExistentID)
		seedTenant(t, ctx, rawPool, "tenant-setrealm-notfound")
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.SetRealmFields(tctx, nonExistentID, "r1", domain.RealmShared, "shard1", 1)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrTenantNotFound.Error(), de.Code)
	})

	// ERRPATH-TENANT-021 — SetRealmFields optimistic conflict (lines 329,338,339)
	t.Run("SetRealmFields_OptimisticConflict", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-setrealm-conflict")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.SetRealmFields(tctx, tenantID, "r1", domain.RealmShared, "shard1", 9999)
		require.Error(t, err)
		var de *domain.DomainError
		assert.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
	})

	// ERRPATH-TENANT-022 — SetRealmFields ctx cancelled (line 326)
	t.Run("SetRealmFields_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-setrealm-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.SetRealmFields(cancelCtx, tenantID, "r1", domain.RealmShared, "shard1", 1)
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-023 — LockSeatOccupancy ctx cancelled (lines 348,351,356)
	t.Run("LockSeatOccupancy_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-lockseat-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		_, err := repo.LockSeatOccupancy(cancelCtx, tenantID)
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-024 — LockForProjection ctx cancelled (lines 381,385,386)
	t.Run("LockForProjection_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-lockproj-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		_, err := repo.LockForProjection(cancelCtx, tenantID)
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-025 — LockForProjection not found → nil, nil (lines 382,383,384)
	t.Run("LockForProjection_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		nonExistentID := uuid.New()
		tctx := withTenant(ctx, nonExistentID)
		seedTenant(t, ctx, rawPool, "tenant-lockproj-notfound")
		repo := pgadapter.NewTenantRepository(appPool)
		lock, err := repo.LockForProjection(tctx, nonExistentID)
		require.NoError(t, err, "not found on LockForProjection must return nil, nil")
		assert.Nil(t, lock)
	})

	// ERRPATH-TENANT-026 — ApplyLifecyclePatch ctx cancelled (lines 408,409)
	t.Run("ApplyLifecyclePatch_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-lifecycle-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		_, err := repo.ApplyLifecyclePatch(cancelCtx, tenantID, port.TenantLifecyclePatch{
			Op:   port.LifecycleSetPlan,
			Plan: domain.PlanPro,
		})
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-027 — ApplyLifecyclePatch LifecycleSetRealm branch (lines 424,427,428)
	t.Run("ApplyLifecyclePatch_SetRealm", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-lifecycle-setrealm")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewTenantRepository(appPool)
		rows, err := repo.ApplyLifecyclePatch(tctx, tenantID, port.TenantLifecyclePatch{
			Op:            port.LifecycleSetRealm,
			RealmID:       "realm-test-001",
			RealmType:     "shared",
			KeycloakShard: "shard-1",
		})
		require.NoError(t, err)
		assert.EqualValues(t, 1, rows, "SetRealm must affect 1 row")
	})

	// ERRPATH-TENANT-028 — ApplyLifecyclePatch LifecycleActivatePaid branch (lines 428,432)
	t.Run("ApplyLifecyclePatch_ActivatePaid", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-lifecycle-activatepaid")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewTenantRepository(appPool)
		rows, err := repo.ApplyLifecyclePatch(tctx, tenantID, port.TenantLifecyclePatch{
			Op:   port.LifecycleActivatePaid,
			Plan: domain.PlanPro,
		})
		require.NoError(t, err)
		assert.EqualValues(t, 1, rows, "ActivatePaid must affect 1 row")
	})

	// ERRPATH-TENANT-029 — ApplyLifecyclePatch unknown op → error (lines 476,477)
	t.Run("ApplyLifecyclePatch_UnknownOp", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-lifecycle-unknown")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewTenantRepository(appPool)
		_, err := repo.ApplyLifecyclePatch(tctx, tenantID, port.TenantLifecyclePatch{
			Op: port.TenantLifecycleOp(9999),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown tenant lifecycle op")
	})

	// ERRPATH-TENANT-030 — WipeTenantChildren ctx cancelled (lines 498,500)
	t.Run("WipeTenantChildren_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-wipe-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.WipeTenantChildren(cancelCtx, tenantID)
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-031 — SetLastEventAt ctx cancelled (line 399)
	t.Run("SetLastEventAt_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-lastevent-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.SetLastEventAt(cancelCtx, tenantID, time.Now())
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-032 — LicensedSeatsForUpdate ctx cancelled (line 276)
	t.Run("LicensedSeatsForUpdate_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-licensedseats-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		_, err := repo.LicensedSeatsForUpdate(cancelCtx, tenantID)
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-033 — ClearOwnerlessSince ctx cancelled (line 305)
	t.Run("ClearOwnerlessSince_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-clearownerless-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.ClearOwnerlessSince(cancelCtx, tenantID)
		assert.Error(t, err)
	})

	// ERRPATH-TENANT-034 — SetOverageSince ctx cancelled (line 367)
	t.Run("SetOverageSince_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "tenant-overage-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		now := time.Now()
		repo := pgadapter.NewTenantRepository(appPool)
		err := repo.SetOverageSince(cancelCtx, tenantID, &now)
		assert.Error(t, err)
	})
}

// ═══════════════════════════════════════════════════════════════════════════
// ReconcilerStore — error paths  (one shared container, 5 subtests)
// ═══════════════════════════════════════════════════════════════════════════

func TestErrPaths_ReconcilerStore(t *testing.T) {
	t.Parallel()
	_, _, sysPool := setupTestDB(t)

	// ERRPATH-RECON-001 — ListSeatOverageCandidates ctx cancelled (lines 29,34,40)
	t.Run("ListSeatOverageCandidates_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		store := pgadapter.NewReconcilerStore(sysPool)
		_, err := store.ListSeatOverageCandidates(cancelCtx, 10)
		assert.Error(t, err)
	})

	// ERRPATH-RECON-002 — ListRealmSyncPending ctx cancelled (lines 52,58,64)
	t.Run("ListRealmSyncPending_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		store := pgadapter.NewReconcilerStore(sysPool)
		_, err := store.ListRealmSyncPending(cancelCtx, 10)
		assert.Error(t, err)
	})

	// ERRPATH-RECON-003 — HardDeleteExpiredTrials ctx cancelled (lines 84,89)
	t.Run("HardDeleteExpiredTrials_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		store := pgadapter.NewReconcilerStore(sysPool)
		_, err := store.HardDeleteExpiredTrials(cancelCtx, 30)
		assert.Error(t, err)
	})

	// ERRPATH-RECON-004 — removed 2026-09-20: PruneOutbox (and its hand-rolled
	// DELETE against outbox_events) no longer exists — pruning goes entirely
	// through platform-events' outbox.Runner.PrunePublished now.

	// ERRPATH-RECON-005 — PruneProcessedEvents ctx cancelled (lines 120,128)
	t.Run("PruneProcessedEvents_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		store := pgadapter.NewReconcilerStore(sysPool)
		_, err := store.PruneProcessedEvents(cancelCtx, 8, 100)
		assert.Error(t, err)
	})
}

// ═══════════════════════════════════════════════════════════════════════════
// IdempotencyRepository — error paths  (one shared container, 2 subtests)
// ═══════════════════════════════════════════════════════════════════════════

func TestErrPaths_IdempotencyRepo(t *testing.T) {
	t.Parallel()
	appPool, _, _ := setupTestDB(t)

	// ERRPATH-IDEMP-001 — IsProcessed ctx cancelled (lines 31,37)
	t.Run("IsProcessed_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		repo := pgadapter.NewIdempotencyRepository(appPool)
		result, err := repo.IsProcessed(cancelCtx, "test-consumer", "test-event-id")
		assert.False(t, result)
		assert.Error(t, err, "cancelled context must propagate as error from IsProcessed")
	})

	// ERRPATH-IDEMP-002 — MarkProcessed ctx cancelled (line 47)
	t.Run("MarkProcessed_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		repo := pgadapter.NewIdempotencyRepository(appPool)
		err := repo.MarkProcessed(cancelCtx, "test-consumer", "test-event-id")
		assert.Error(t, err)
	})
}

// ═══════════════════════════════════════════════════════════════════════════
// AuthZRepository — error paths  (one shared container, 4 subtests)
// ═══════════════════════════════════════════════════════════════════════════

func TestErrPaths_AuthZRepo(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)

	// ERRPATH-AUTHZ-001 — FindMembershipProjection not found → nil, nil (lines 52,53,54,56)
	t.Run("FindMembershipProjection_NotFound", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "authz-notfound")
		tctx := withTenant(ctx, tenantID)
		repo := pgadapter.NewAuthZRepository(appPool)
		proj, err := repo.FindMembershipProjection(tctx, tenantID, uuid.New())
		require.NoError(t, err, "not-found in AuthZ must return nil, nil (caller maps to 404)")
		assert.Nil(t, proj)
	})

	// ERRPATH-AUTHZ-002 — FindMembershipProjection ctx cancelled (lines 44,56)
	t.Run("FindMembershipProjection_CtxCancelled", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "authz-ctx")
		tctx := withTenant(ctx, tenantID)
		cancelCtx, cancel := context.WithCancel(tctx)
		cancel()
		repo := pgadapter.NewAuthZRepository(appPool)
		_, err := repo.FindMembershipProjection(cancelCtx, tenantID, uuid.New())
		assert.Error(t, err)
	})

	// ERRPATH-AUTHZ-003 — FindMembershipProjection with roles and depts (lines 59,64,70,77,86,92,100,106,124)
	t.Run("FindMembershipProjection_WithRolesAndDepts", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "authz-with-roles")
		userID := uuid.New()
		memID := uuid.New()
		deptID := uuid.New()
		tctx := withTenant(ctx, tenantID)

		_, err := rawPool.Exec(ctx,
			`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
			memID, tenantID, userID)
		require.NoError(t, err)
		_, err = rawPool.Exec(ctx,
			`INSERT INTO tenant_roles (id, tenant_id, user_id, tenant_membership_id, role_code, granted_by) VALUES (gen_random_uuid(), $1, $2, $3, 'tenant_admin', $2)`,
			tenantID, userID, memID)
		require.NoError(t, err)
		_, err = rawPool.Exec(ctx,
			`INSERT INTO tenant_departments (tenant_id, department_id, is_active) VALUES ($1, $2, true)`,
			tenantID, deptID)
		require.NoError(t, err)
		_, err = rawPool.Exec(ctx,
			`INSERT INTO dept_memberships (id, tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by) VALUES (gen_random_uuid(), $1, $2, $3, $4, 'preparator', $2)`,
			tenantID, userID, memID, deptID)
		require.NoError(t, err)

		repo := pgadapter.NewAuthZRepository(appPool)
		proj, err := repo.FindMembershipProjection(tctx, tenantID, userID)
		require.NoError(t, err)
		require.NotNil(t, proj)
		assert.Equal(t, domain.MembershipActive, proj.MembershipStatus)
		assert.Len(t, proj.Roles, 1)
		assert.Equal(t, domain.TenantRoleCode("tenant_admin"), proj.Roles[0])
		assert.Len(t, proj.Departments, 1)
		assert.Equal(t, deptID, proj.Departments[0].DepartmentID)
	})

	// ERRPATH-AUTHZ-004 — FindMembershipProjection member exists no roles/depts (lines 67,68,77,86,100,106,124)
	t.Run("FindMembershipProjection_NoRolesNoDepts", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		tenantID := seedTenant(t, ctx, rawPool, "authz-no-roles")
		userID := uuid.New()
		tctx := withTenant(ctx, tenantID)

		_, err := rawPool.Exec(ctx,
			`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES (gen_random_uuid(), $1, $2, 'active')`,
			tenantID, userID)
		require.NoError(t, err)

		repo := pgadapter.NewAuthZRepository(appPool)
		proj, err := repo.FindMembershipProjection(tctx, tenantID, userID)
		require.NoError(t, err)
		require.NotNil(t, proj)
		assert.Empty(t, proj.Roles)
		assert.Empty(t, proj.Departments)
	})
}
