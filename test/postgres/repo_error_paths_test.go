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
// All tests use the shared testcontainer infrastructure (setupTestDB,
// seedTenant, withTenant) and call pgadapter.New* constructors.
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
// InvitationRepository — error paths
// ═══════════════════════════════════════════════════════════════════════════

// Test Case ID:      ERRPATH-INV-001
// Feature:           InvitationRepository.List — context cancelled
// Lines:             invitation_repository.go:62,68
// Technique:         Cancel ctx before call → DB query fails
func TestErrPath_Invitation_List_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-list-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	_, err := repo.List(cancelCtx, tenantID)
	assert.Error(t, err, "cancelled context must return an error")
}

// Test Case ID:      ERRPATH-INV-002
// Feature:           InvitationRepository.LockByID — not found returns nil
// Lines:             invitation_repository.go:82,83,86
// Technique:         Non-existent ID → ErrNoRows → nil, nil
func TestErrPath_Invitation_LockByID_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-lock-notfound")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewInvitationRepository(appPool)
	inv, err := repo.LockByID(tctx, uuid.New())
	require.NoError(t, err, "LockByID with non-existent ID must not error")
	assert.Nil(t, inv, "LockByID with non-existent ID must return nil invitation")
}

// Test Case ID:      ERRPATH-INV-003
// Feature:           InvitationRepository.LockByID — context cancelled
// Lines:             invitation_repository.go:82
// Technique:         Cancel ctx → DB call fails
func TestErrPath_Invitation_LockByID_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-lock-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	_, err := repo.LockByID(cancelCtx, uuid.New())
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-INV-004
// Feature:           InvitationRepository.FindByID — not found → ErrInvitationNotFound
// Lines:             invitation_repository.go:100,101,103
// Technique:         Non-existent ID → ErrNoRows → domain error
func TestErrPath_Invitation_FindByID_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-INV-005
// Feature:           InvitationRepository.FindByID — context cancelled
// Lines:             invitation_repository.go:98
// Technique:         Cancel ctx → DB call fails
func TestErrPath_Invitation_FindByID_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-findbyid-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	_, err := repo.FindByID(cancelCtx, tenantID, uuid.New())
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-INV-006
// Feature:           InvitationRepository.FindPendingByEmail — context cancelled
// Lines:             invitation_repository.go:117,123
// Technique:         Cancel ctx → DB call fails
func TestErrPath_Invitation_FindPendingByEmail_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-email-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	_, err := repo.FindPendingByEmail(cancelCtx, tenantID, "test@example.com")
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-INV-007
// Feature:           InvitationRepository.FindPendingByKeycloakUser — not found returns nil
// Lines:             invitation_repository.go:136,140,142
// Technique:         Non-existent KC user ID → nil, nil
func TestErrPath_Invitation_FindPendingByKeycloakUser_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-kc-notfound")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewInvitationRepository(appPool)
	inv, err := repo.FindPendingByKeycloakUser(tctx, tenantID, uuid.New())
	require.NoError(t, err)
	assert.Nil(t, inv)
}

// Test Case ID:      ERRPATH-INV-008
// Feature:           InvitationRepository.FindPendingByKeycloakUser — context cancelled
// Lines:             invitation_repository.go:134
// Technique:         Cancel ctx → DB call fails
func TestErrPath_Invitation_FindPendingByKeycloakUser_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-kc-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	_, err := repo.FindPendingByKeycloakUser(cancelCtx, tenantID, uuid.New())
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-INV-009
// Feature:           InvitationRepository.Insert — context cancelled
// Lines:             invitation_repository.go:168,186
// Technique:         Cancel ctx → INSERT fails
func TestErrPath_Invitation_Insert_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-INV-010
// Feature:           InvitationRepository.SetKeycloakUserID — not-found after 0 rows
// Lines:             invitation_repository.go:199,209
// Technique:         Non-existent ID → 0 rows affected → probe finds no row → ErrInvitationNotFound
func TestErrPath_Invitation_SetKeycloakUserID_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-setkc-notfound")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewInvitationRepository(appPool)
	err := repo.SetKeycloakUserID(tctx, tenantID, uuid.New(), uuid.New(), 1)
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrInvitationNotFound.Error(), de.Code)
}

// Test Case ID:      ERRPATH-INV-011
// Feature:           InvitationRepository.SetKeycloakUserID — optimistic lock conflict
// Lines:             invitation_repository.go:199,211
// Technique:         Existing invitation + wrong version → 0 rows → probe finds row → ErrOptimisticLockConflict
func TestErrPath_Invitation_SetKeycloakUserID_OptimisticConflict(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-setkc-conflict")
	tctx := withTenant(ctx, tenantID)

	// Insert an invitation directly via rawPool (bypass RLS)
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
}

// Test Case ID:      ERRPATH-INV-012
// Feature:           InvitationRepository.SetKeycloakUserID — context cancelled
// Lines:             invitation_repository.go:196
// Technique:         Cancel ctx → UPDATE fails
func TestErrPath_Invitation_SetKeycloakUserID_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-setkc-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	err := repo.SetKeycloakUserID(cancelCtx, tenantID, uuid.New(), uuid.New(), 1)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-INV-013
// Feature:           InvitationRepository.SetStatus — not-found (non-existent ID)
// Lines:             invitation_repository.go:232,237,238,240
// Technique:         Non-existent ID → 0 rows → probe returns ErrNoRows → ErrInvitationNotFound
func TestErrPath_Invitation_SetStatus_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-setstatus-notfound")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewInvitationRepository(appPool)
	_, err := repo.SetStatus(tctx, tenantID, uuid.New(), domain.InviteAccepted, 1)
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrInvitationNotFound.Error(), de.Code)
}

// Test Case ID:      ERRPATH-INV-014
// Feature:           InvitationRepository.SetStatus — terminal state → ErrInvitationNotFound
// Lines:             invitation_repository.go:240,241,242,243,244,249
// Technique:         Invitation in terminal state (revoked/expired) → not-found per LLD P-31
func TestErrPath_Invitation_SetStatus_TerminalState(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-setstatus-terminal")
	tctx := withTenant(ctx, tenantID)

	// Insert with a future expires_at (trigger only checks on INSERT) then UPDATE status
	// to a terminal value via rawPool to bypass the SetStatus pending-only predicate.
	invID := uuid.New()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, initial_tenant_roles,
			initial_dept_mappings, invited_by, status, expires_at, kc_cleanup_pending)
		VALUES ($1, $2, 'terminal@example.com', 'Terminal User', '{}', '[]',
			$3, 'pending', now() + interval '7 days', false)`,
		invID, tenantID, uuid.New())
	require.NoError(t, err)
	// Flip to 'expired' after insert (trigger only fires on INSERT)
	_, err = rawPool.Exec(ctx,
		`UPDATE pending_invitations SET status = 'expired' WHERE id = $1`, invID)
	require.NoError(t, err)

	repo := pgadapter.NewInvitationRepository(appPool)
	// Try to set status on an expired invitation → should return ErrInvitationNotFound
	_, err = repo.SetStatus(tctx, tenantID, invID, domain.InviteAccepted, 1)
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrInvitationNotFound.Error(), de.Code)
}

// Test Case ID:      ERRPATH-INV-015
// Feature:           InvitationRepository.SetStatus — optimistic lock conflict
// Lines:             invitation_repository.go:232,246,247
// Technique:         Existing pending invitation + wrong version → probe finds row → ErrOptimisticLockConflict
func TestErrPath_Invitation_SetStatus_OptimisticConflict(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-INV-016
// Feature:           InvitationRepository.SetStatus — context cancelled
// Lines:             invitation_repository.go:225
// Technique:         Cancel ctx → UPDATE fails
func TestErrPath_Invitation_SetStatus_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-setstatus-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	_, err := repo.SetStatus(cancelCtx, tenantID, uuid.New(), domain.InviteAccepted, 1)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-INV-017
// Feature:           InvitationRepository.SetKCCleanupPending — not-found after 0 rows
// Lines:             invitation_repository.go:262,269,272
// Technique:         Non-existent ID → 0 rows → probe finds no row → ErrInvitationNotFound
func TestErrPath_Invitation_SetKCCleanupPending_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-kcleanup-notfound")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewInvitationRepository(appPool)
	err := repo.SetKCCleanupPending(tctx, tenantID, uuid.New(), true, 1)
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrInvitationNotFound.Error(), de.Code)
}

// Test Case ID:      ERRPATH-INV-018
// Feature:           InvitationRepository.SetKCCleanupPending — optimistic lock conflict
// Lines:             invitation_repository.go:262,274
// Technique:         Existing invitation + wrong version → probe finds row → ErrOptimisticLockConflict
func TestErrPath_Invitation_SetKCCleanupPending_OptimisticConflict(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-INV-019
// Feature:           InvitationRepository.SetKCCleanupPending — context cancelled
// Lines:             invitation_repository.go:259
// Technique:         Cancel ctx → UPDATE fails
func TestErrPath_Invitation_SetKCCleanupPending_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-kcleanup-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	err := repo.SetKCCleanupPending(cancelCtx, tenantID, uuid.New(), true, 1)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-INV-020
// Feature:           InvitationRepository.ListExpiring — context cancelled
// Lines:             invitation_repository.go:293,299
// Technique:         Cancel ctx → Query fails
func TestErrPath_Invitation_ListExpiring_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-listexpiring-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	_, err := repo.ListExpiring(cancelCtx, time.Now().Add(24*time.Hour), 10)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-INV-021
// Feature:           InvitationRepository.ListPendingKCCleanup — context cancelled
// Lines:             invitation_repository.go:313,319
// Technique:         Cancel ctx → Query fails
func TestErrPath_Invitation_ListPendingKCCleanup_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-pendingkc-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	_, err := repo.ListPendingKCCleanup(cancelCtx, 10)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-INV-022
// Feature:           InvitationRepository.ExpireOverdue — context cancelled
// Lines:             invitation_repository.go:372
// Technique:         Cancel ctx → Exec fails
func TestErrPath_Invitation_ExpireOverdue_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-expire-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	_, err := repo.ExpireOverdue(cancelCtx, 100)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-INV-023
// Feature:           InvitationRepository.MostRecentCreatedAt — context cancelled
// Lines:             invitation_repository.go:333
// Technique:         Cancel ctx → Query fails
func TestErrPath_Invitation_MostRecentCreatedAt_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-mostrecent-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	_, err := repo.MostRecentCreatedAt(cancelCtx, tenantID, "test@example.com")
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-INV-024
// Feature:           InvitationRepository.CountCreatedInWindow — context cancelled
// Lines:             invitation_repository.go:351
// Technique:         Cancel ctx → Query fails
func TestErrPath_Invitation_CountCreatedInWindow_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-countwindow-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	_, err := repo.CountCreatedInWindow(cancelCtx, tenantID, time.Now().Add(-time.Hour))
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-INV-025
// Feature:           InvitationRepository.CountPending — context cancelled
// Lines:             invitation_repository.go:283
// Technique:         Cancel ctx → Query fails
func TestErrPath_Invitation_CountPending_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "inv-countpending-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewInvitationRepository(appPool)
	_, err := repo.CountPending(cancelCtx, tenantID)
	assert.Error(t, err)
}

// ═══════════════════════════════════════════════════════════════════════════
// MembershipRepository — error paths
// ═══════════════════════════════════════════════════════════════════════════

// Test Case ID:      ERRPATH-MEM-001
// Feature:           MembershipRepository.List — context cancelled (no cursor)
// Lines:             membership_repository.go:64,77
// Technique:         Cancel ctx → Query fails
func TestErrPath_Membership_List_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "mem-list-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewMembershipRepository(appPool)
	_, err := repo.List(cancelCtx, tenantID, nil, 10)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-MEM-002
// Feature:           MembershipRepository.List — context cancelled (with cursor)
// Lines:             membership_repository.go:64,77
// Technique:         Cancel ctx with cursor set → cursor branch fails
func TestErrPath_Membership_List_WithCursor_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "mem-list-cursor-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewMembershipRepository(appPool)
	cursor := &domain.MembershipListCursor{CreatedAt: time.Now(), ID: uuid.New()}
	_, err := repo.List(cancelCtx, tenantID, cursor, 10)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-MEM-003
// Feature:           MembershipRepository.FindByUserID — not found → ErrMemberNotFound
// Lines:             membership_repository.go:99,100,102
// Technique:         Non-existent user ID → ErrNoRows → domain error
func TestErrPath_Membership_FindByUserID_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-MEM-004
// Feature:           MembershipRepository.FindByUserID — context cancelled
// Lines:             membership_repository.go:96
// Technique:         Cancel ctx → Query fails
func TestErrPath_Membership_FindByUserID_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "mem-findbyuser-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewMembershipRepository(appPool)
	_, err := repo.FindByUserID(cancelCtx, tenantID, uuid.New())
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-MEM-005
// Feature:           MembershipRepository.Insert — context cancelled
// Lines:             membership_repository.go:124,132
// Technique:         Cancel ctx → INSERT fails
func TestErrPath_Membership_Insert_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-MEM-006
// Feature:           MembershipRepository.SetStatus — not found → probeMembership → ErrMemberNotFound
// Lines:             membership_repository.go:167,168,170
// Technique:         Non-existent user ID → 0 rows → probe finds nothing → ErrMemberNotFound
func TestErrPath_Membership_SetStatus_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "mem-setstatus-notfound")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewMembershipRepository(appPool)
	_, err := repo.SetStatus(tctx, tenantID, uuid.New(), domain.MembershipSuspended, 1)
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrMemberNotFound.Error(), de.Code)
}

// Test Case ID:      ERRPATH-MEM-007
// Feature:           MembershipRepository.SetStatus — optimistic lock conflict
// Lines:             membership_repository.go:167,168,184
// Technique:         Existing membership + wrong record_version → probe finds row → ErrOptimisticLockConflict
func TestErrPath_Membership_SetStatus_OptimisticConflict(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "mem-setstatus-conflict")
	userID := uuid.New()
	tctx := withTenant(ctx, tenantID)

	_, err := rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES (gen_random_uuid(), $1, $2, 'active')`,
		tenantID, userID)
	require.NoError(t, err)

	repo := pgadapter.NewMembershipRepository(appPool)
	_, err = repo.SetStatus(tctx, tenantID, userID, domain.MembershipSuspended, 9999) // wrong version
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
}

// Test Case ID:      ERRPATH-MEM-008
// Feature:           MembershipRepository.SetStatus — context cancelled
// Lines:             membership_repository.go:160
// Technique:         Cancel ctx → UPDATE fails
func TestErrPath_Membership_SetStatus_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "mem-setstatus-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewMembershipRepository(appPool)
	_, err := repo.SetStatus(cancelCtx, tenantID, uuid.New(), domain.MembershipSuspended, 1)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-MEM-009
// Feature:           MembershipRepository.SoftDelete — not found → ErrMemberNotFound
// Lines:             membership_repository.go:184,187,188,210,216,233
// Technique:         Non-existent user ID → 0 rows → probe → ErrMemberNotFound
func TestErrPath_Membership_SoftDelete_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "mem-softdel-notfound")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewMembershipRepository(appPool)
	err := repo.SoftDelete(tctx, tenantID, uuid.New(), 1)
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrMemberNotFound.Error(), de.Code)
}

// Test Case ID:      ERRPATH-MEM-010
// Feature:           MembershipRepository.SoftDelete — optimistic lock conflict
// Lines:             membership_repository.go:184,187,188,233
// Technique:         Existing membership + wrong version → probe finds row → ErrOptimisticLockConflict
func TestErrPath_Membership_SoftDelete_OptimisticConflict(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "mem-softdel-conflict")
	userID := uuid.New()
	tctx := withTenant(ctx, tenantID)

	_, err := rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES (gen_random_uuid(), $1, $2, 'active')`,
		tenantID, userID)
	require.NoError(t, err)

	repo := pgadapter.NewMembershipRepository(appPool)
	err = repo.SoftDelete(tctx, tenantID, userID, 9999) // wrong version
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
}

// Test Case ID:      ERRPATH-MEM-011
// Feature:           MembershipRepository.SoftDelete — context cancelled
// Lines:             membership_repository.go:180
// Technique:         Cancel ctx → Exec fails
func TestErrPath_Membership_SoftDelete_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "mem-softdel-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewMembershipRepository(appPool)
	err := repo.SoftDelete(cancelCtx, tenantID, uuid.New(), 1)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-MEM-012
// Feature:           MembershipRepository.ListActiveUserIDs — context cancelled
// Lines:             membership_repository.go:210
// Technique:         Cancel ctx → Query fails
func TestErrPath_Membership_ListActiveUserIDs_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "mem-listuids-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewMembershipRepository(appPool)
	_, err := repo.ListActiveUserIDs(cancelCtx, tenantID)
	assert.Error(t, err)
}

// ═══════════════════════════════════════════════════════════════════════════
// TenantRepository — error paths
// ═══════════════════════════════════════════════════════════════════════════

// Test Case ID:      ERRPATH-TENANT-001
// Feature:           TenantRepository.FindByID — not found → ErrTenantNotFound
// Lines:             tenant_repository.go:47,48,51
// Technique:         Non-existent ID → ErrNoRows → domain error
func TestErrPath_Tenant_FindByID_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-findbyid-notfound")
	_ = tenantID
	tctx := withTenant(ctx, uuid.New()) // set GUC to unknown tenant

	repo := pgadapter.NewTenantRepository(appPool)
	ten, err := repo.FindByID(tctx, uuid.New())
	assert.Nil(t, ten)
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrTenantNotFound.Error(), de.Code)
}

// Test Case ID:      ERRPATH-TENANT-002
// Feature:           TenantRepository.FindByID — context cancelled
// Lines:             tenant_repository.go:47
// Technique:         Cancel ctx → Query fails
func TestErrPath_Tenant_FindByID_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-findbyid-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewTenantRepository(appPool)
	_, err := repo.FindByID(cancelCtx, tenantID)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-003
// Feature:           TenantRepository.FindByIDIncludingDeleted — not found → ErrTenantNotFound
// Lines:             tenant_repository.go:70,71,73
// Technique:         Non-existent ID → ErrNoRows → domain error
func TestErrPath_Tenant_FindByIDIncludingDeleted_NotFound(t *testing.T) {
	t.Parallel()
	_, _, sysPool := setupTestDB(t)
	ctx := context.Background()

	// Use sysPool (BYPASSRLS) because this function searches without deleted_at filter
	repo := pgadapter.NewTenantRepository(sysPool)
	ten, err := repo.FindByIDIncludingDeleted(ctx, uuid.New())
	assert.Nil(t, ten)
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrTenantNotFound.Error(), de.Code)
}

// Test Case ID:      ERRPATH-TENANT-004
// Feature:           TenantRepository.FindByIDIncludingDeleted — context cancelled
// Lines:             tenant_repository.go:67
// Technique:         Cancel ctx → Query fails
func TestErrPath_Tenant_FindByIDIncludingDeleted_CtxCancelled(t *testing.T) {
	t.Parallel()
	_, _, sysPool := setupTestDB(t)
	ctx := context.Background()

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	repo := pgadapter.NewTenantRepository(sysPool)
	_, err := repo.FindByIDIncludingDeleted(cancelCtx, uuid.New())
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-005
// Feature:           TenantRepository.Update — nil patch → ErrValidation
// Lines:             tenant_repository.go:90,91
// Technique:         Pass nil patch → immediate validation error
func TestErrPath_Tenant_Update_NilPatch(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-TENANT-006
// Feature:           TenantRepository.Update — empty patch (no fields set) → returns current row
// Lines:             tenant_repository.go:114,115,116
// Technique:         All optional fields nil → len(sets)==0 → falls through to FindByID
func TestErrPath_Tenant_Update_EmptyPatch(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-update-emptypatch")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantRepository(appPool)
	// RecordVersion=1 but no other fields set → len(sets)==0 → FindByID path
	patch := &domain.TenantPatch{RecordVersion: 1}
	ten, err := repo.Update(tctx, tenantID, patch)
	require.NoError(t, err)
	assert.NotNil(t, ten, "empty patch must return current tenant row")
}

// Test Case ID:      ERRPATH-TENANT-007
// Feature:           TenantRepository.Update — wrong record version → ErrOptimisticLockConflict
// Lines:             tenant_repository.go:127,128,133
// Technique:         Existing tenant + wrong record_version in patch → optimistic lock conflict
func TestErrPath_Tenant_Update_OptimisticConflict(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-update-conflict")
	tctx := withTenant(ctx, tenantID)

	name := "Updated Name"
	repo := pgadapter.NewTenantRepository(appPool)
	patch := &domain.TenantPatch{RecordVersion: 9999, Name: &name} // wrong version
	_, err := repo.Update(tctx, tenantID, patch)
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
}

// Test Case ID:      ERRPATH-TENANT-008
// Feature:           TenantRepository.Update — not found → ErrTenantNotFound
// Lines:             tenant_repository.go:127,128,133
// Technique:         Non-existent tenant ID → 0 rows → probe finds nothing → ErrTenantNotFound
func TestErrPath_Tenant_Update_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	nonExistentID := uuid.New()
	tctx := withTenant(ctx, nonExistentID) // GUC set to unknown tenant

	// We need to seed a tenant to pass pool setup but query against different ID
	seedTenant(t, ctx, rawPool, "tenant-update-notfound")

	name := "Ghost Tenant"
	repo := pgadapter.NewTenantRepository(appPool)
	patch := &domain.TenantPatch{RecordVersion: 1, Name: &name}
	_, err := repo.Update(tctx, nonExistentID, patch)
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrTenantNotFound.Error(), de.Code)
}

// Test Case ID:      ERRPATH-TENANT-009
// Feature:           TenantRepository.SetRealmSyncPending — not found (0 rows affected)
// Lines:             tenant_repository.go:151,154
// Technique:         Non-existent tenant ID → Exec runs but 0 rows affected → ErrTenantNotFound
func TestErrPath_Tenant_SetRealmSyncPending_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-TENANT-010
// Feature:           TenantRepository.SetRealmSyncPending — context cancelled
// Lines:             tenant_repository.go:148
// Technique:         Cancel ctx → Exec fails
func TestErrPath_Tenant_SetRealmSyncPending_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-realmsyncp-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewTenantRepository(appPool)
	err := repo.SetRealmSyncPending(cancelCtx, tenantID)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-011
// Feature:           TenantRepository.Insert — nil tenant → ErrValidation
// Lines:             tenant_repository.go:169,173
// Technique:         Pass nil → immediate validation error
func TestErrPath_Tenant_Insert_NilTenant(t *testing.T) {
	t.Parallel()
	appPool, _, _ := setupTestDB(t)
	ctx := context.Background()

	repo := pgadapter.NewTenantRepository(appPool)
	out, wasCreated, err := repo.Insert(ctx, nil)
	assert.Nil(t, out)
	assert.False(t, wasCreated)
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrValidation.Error(), de.Code)
}

// Test Case ID:      ERRPATH-TENANT-012
// Feature:           TenantRepository.Insert — context cancelled
// Lines:             tenant_repository.go:185
// Technique:         Cancel ctx → INSERT fails
func TestErrPath_Tenant_Insert_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, _, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-TENANT-013
// Feature:           TenantRepository.ListSubscriptionLapses — context cancelled
// Lines:             tenant_repository.go:233,241,247,254
// Technique:         Cancel ctx → Query fails
func TestErrPath_Tenant_ListSubscriptionLapses_CtxCancelled(t *testing.T) {
	t.Parallel()
	_, _, sysPool := setupTestDB(t)
	ctx := context.Background()

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	repo := pgadapter.NewTenantRepository(sysPool)
	_, err := repo.ListSubscriptionLapses(cancelCtx, 30)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-014
// Feature:           TenantRepository.LockByID — not found → ErrTenantNotFound
// Lines:             tenant_repository.go:263,266,267
// Technique:         Non-existent ID → 0 rows → ErrTenantNotFound
func TestErrPath_Tenant_LockByID_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-TENANT-015
// Feature:           TenantRepository.LockByID — context cancelled
// Lines:             tenant_repository.go:262
// Technique:         Cancel ctx → Exec fails
func TestErrPath_Tenant_LockByID_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-lockbyid-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewTenantRepository(appPool)
	err := repo.LockByID(cancelCtx, tenantID)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-016
// Feature:           TenantRepository.SetFeatureFlags — not found → ErrTenantNotFound
// Lines:             tenant_repository.go:286,293,295
// Technique:         Non-existent ID → 0 rows → probe returns no row → ErrTenantNotFound
func TestErrPath_Tenant_SetFeatureFlags_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-TENANT-017
// Feature:           TenantRepository.SetFeatureFlags — optimistic lock conflict
// Lines:             tenant_repository.go:286,297,298
// Technique:         Existing tenant + wrong record_version → probe finds row → ErrOptimisticLockConflict
func TestErrPath_Tenant_SetFeatureFlags_OptimisticConflict(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-setflags-conflict")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantRepository(appPool)
	err := repo.SetFeatureFlags(tctx, tenantID, []byte(`{"feature_x": true}`), 9999) // wrong version
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
}

// Test Case ID:      ERRPATH-TENANT-018
// Feature:           TenantRepository.SetFeatureFlags — context cancelled
// Lines:             tenant_repository.go:283
// Technique:         Cancel ctx → Exec fails
func TestErrPath_Tenant_SetFeatureFlags_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-setflags-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewTenantRepository(appPool)
	err := repo.SetFeatureFlags(cancelCtx, tenantID, []byte(`{}`), 1)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-019
// Feature:           TenantRepository.MarkOwnerlessIfUnset — context cancelled
// Lines:             tenant_repository.go:314
// Technique:         Cancel ctx → Exec fails
func TestErrPath_Tenant_MarkOwnerlessIfUnset_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-ownerless-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewTenantRepository(appPool)
	_, err := repo.MarkOwnerlessIfUnset(cancelCtx, tenantID)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-020
// Feature:           TenantRepository.SetRealmFields — not found → ErrTenantNotFound
// Lines:             tenant_repository.go:329,333,336
// Technique:         Non-existent ID → 0 rows → probe finds nothing → ErrTenantNotFound
func TestErrPath_Tenant_SetRealmFields_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-TENANT-021
// Feature:           TenantRepository.SetRealmFields — optimistic lock conflict
// Lines:             tenant_repository.go:329,338,339
// Technique:         Existing tenant + wrong version → probe finds row → ErrOptimisticLockConflict
func TestErrPath_Tenant_SetRealmFields_OptimisticConflict(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-setrealm-conflict")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantRepository(appPool)
	err := repo.SetRealmFields(tctx, tenantID, "r1", domain.RealmShared, "shard1", 9999) // wrong version
	require.Error(t, err)
	var de *domain.DomainError
	assert.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrOptimisticLockConflict.Error(), de.Code)
}

// Test Case ID:      ERRPATH-TENANT-022
// Feature:           TenantRepository.SetRealmFields — context cancelled
// Lines:             tenant_repository.go:326
// Technique:         Cancel ctx → Exec fails
func TestErrPath_Tenant_SetRealmFields_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-setrealm-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewTenantRepository(appPool)
	err := repo.SetRealmFields(cancelCtx, tenantID, "r1", domain.RealmShared, "shard1", 1)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-023
// Feature:           TenantRepository.LockSeatOccupancy — context cancelled
// Lines:             tenant_repository.go:348,351,356
// Technique:         Cancel ctx → QueryRow.Scan fails
func TestErrPath_Tenant_LockSeatOccupancy_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-lockseat-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewTenantRepository(appPool)
	_, err := repo.LockSeatOccupancy(cancelCtx, tenantID)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-024
// Feature:           TenantRepository.LockForProjection — context cancelled
// Lines:             tenant_repository.go:381,385,386
// Technique:         Cancel ctx → QueryRow.Scan fails
func TestErrPath_Tenant_LockForProjection_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-lockproj-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewTenantRepository(appPool)
	_, err := repo.LockForProjection(cancelCtx, tenantID)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-025
// Feature:           TenantRepository.LockForProjection — tenant not found → returns nil (no error)
// Lines:             tenant_repository.go:382,383,384
// Technique:         Non-existent tenant → ErrNoRows → nil lock, nil error
func TestErrPath_Tenant_LockForProjection_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	nonExistentID := uuid.New()
	tctx := withTenant(ctx, nonExistentID)
	seedTenant(t, ctx, rawPool, "tenant-lockproj-notfound")

	repo := pgadapter.NewTenantRepository(appPool)
	lock, err := repo.LockForProjection(tctx, nonExistentID)
	require.NoError(t, err, "not found on LockForProjection must return nil, nil")
	assert.Nil(t, lock)
}

// Test Case ID:      ERRPATH-TENANT-026
// Feature:           TenantRepository.ApplyLifecyclePatch — context cancelled
// Lines:             tenant_repository.go:408,409
// Technique:         Cancel ctx → execLifecyclePatch Exec fails
func TestErrPath_Tenant_ApplyLifecyclePatch_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-TENANT-027
// Feature:           TenantRepository.ApplyLifecyclePatch — LifecycleSetRealm branch
// Lines:             tenant_repository.go:424,427,428
// Technique:         Call with LifecycleSetRealm op → exercises that switch arm
func TestErrPath_Tenant_ApplyLifecyclePatch_SetRealm(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-TENANT-028
// Feature:           TenantRepository.ApplyLifecyclePatch — LifecycleActivatePaid branch
// Lines:             tenant_repository.go:428,432
// Technique:         Call with LifecycleActivatePaid op → exercises that switch arm
func TestErrPath_Tenant_ApplyLifecyclePatch_ActivatePaid(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}

// Test Case ID:      ERRPATH-TENANT-029
// Feature:           TenantRepository.ApplyLifecyclePatch — unknown op → error
// Lines:             tenant_repository.go:476,477
// Technique:         Pass TenantLifecycleOp(9999) → default branch → error
func TestErrPath_Tenant_ApplyLifecyclePatch_UnknownOp(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-lifecycle-unknown")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewTenantRepository(appPool)
	_, err := repo.ApplyLifecyclePatch(tctx, tenantID, port.TenantLifecyclePatch{
		Op: port.TenantLifecycleOp(9999), // unknown op → default branch
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown tenant lifecycle op")
}

// Test Case ID:      ERRPATH-TENANT-030
// Feature:           TenantRepository.WipeTenantChildren — context cancelled
// Lines:             tenant_repository.go:498,500
// Technique:         Cancel ctx → DELETE fails
func TestErrPath_Tenant_WipeTenantChildren_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-wipe-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewTenantRepository(appPool)
	err := repo.WipeTenantChildren(cancelCtx, tenantID)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-031
// Feature:           TenantRepository.SetLastEventAt — context cancelled
// Lines:             tenant_repository.go:399
// Technique:         Cancel ctx → Exec fails
func TestErrPath_Tenant_SetLastEventAt_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-lastevent-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewTenantRepository(appPool)
	err := repo.SetLastEventAt(cancelCtx, tenantID, time.Now())
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-032
// Feature:           TenantRepository.LicensedSeatsForUpdate — context cancelled
// Lines:             tenant_repository.go:276
// Technique:         Cancel ctx → QueryRow fails
func TestErrPath_Tenant_LicensedSeatsForUpdate_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-licensedseats-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewTenantRepository(appPool)
	_, err := repo.LicensedSeatsForUpdate(cancelCtx, tenantID)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-033
// Feature:           TenantRepository.ClearOwnerlessSince — context cancelled
// Lines:             tenant_repository.go:305
// Technique:         Cancel ctx → Exec fails
func TestErrPath_Tenant_ClearOwnerlessSince_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-clearownerless-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewTenantRepository(appPool)
	err := repo.ClearOwnerlessSince(cancelCtx, tenantID)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-TENANT-034
// Feature:           TenantRepository.SetOverageSince — context cancelled
// Lines:             tenant_repository.go:367
// Technique:         Cancel ctx → Exec fails
func TestErrPath_Tenant_SetOverageSince_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "tenant-overage-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	now := time.Now()
	repo := pgadapter.NewTenantRepository(appPool)
	err := repo.SetOverageSince(cancelCtx, tenantID, &now)
	assert.Error(t, err)
}

// ═══════════════════════════════════════════════════════════════════════════
// ReconcilerStore — error paths
// ═══════════════════════════════════════════════════════════════════════════

// Test Case ID:      ERRPATH-RECON-001
// Feature:           ReconcilerStore.ListSeatOverageCandidates — context cancelled
// Lines:             reconciler_store.go:29,34,40
// Technique:         Cancel ctx → Query fails
func TestErrPath_ReconcilerStore_ListSeatOverageCandidates_CtxCancelled(t *testing.T) {
	t.Parallel()
	_, _, sysPool := setupTestDB(t)
	ctx := context.Background()

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	store := pgadapter.NewReconcilerStore(sysPool)
	_, err := store.ListSeatOverageCandidates(cancelCtx, 10)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-RECON-002
// Feature:           ReconcilerStore.ListRealmSyncPending — context cancelled
// Lines:             reconciler_store.go:52,58,64
// Technique:         Cancel ctx → Query fails
func TestErrPath_ReconcilerStore_ListRealmSyncPending_CtxCancelled(t *testing.T) {
	t.Parallel()
	_, _, sysPool := setupTestDB(t)
	ctx := context.Background()

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	store := pgadapter.NewReconcilerStore(sysPool)
	_, err := store.ListRealmSyncPending(cancelCtx, 10)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-RECON-003
// Feature:           ReconcilerStore.HardDeleteExpiredTrials — context cancelled
// Lines:             reconciler_store.go:84,89
// Technique:         Cancel ctx → Exec fails
func TestErrPath_ReconcilerStore_HardDeleteExpiredTrials_CtxCancelled(t *testing.T) {
	t.Parallel()
	_, _, sysPool := setupTestDB(t)
	ctx := context.Background()

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	store := pgadapter.NewReconcilerStore(sysPool)
	_, err := store.HardDeleteExpiredTrials(cancelCtx, 30)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-RECON-004
// Feature:           ReconcilerStore.PruneOutbox — context cancelled
// Lines:             reconciler_store.go:101,109
// Technique:         Cancel ctx → Exec fails
func TestErrPath_ReconcilerStore_PruneOutbox_CtxCancelled(t *testing.T) {
	t.Parallel()
	_, _, sysPool := setupTestDB(t)
	ctx := context.Background()

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	store := pgadapter.NewReconcilerStore(sysPool)
	_, err := store.PruneOutbox(cancelCtx, 8, 100)
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-RECON-005
// Feature:           ReconcilerStore.PruneProcessedEvents — context cancelled
// Lines:             reconciler_store.go:120,128
// Technique:         Cancel ctx → Exec fails
func TestErrPath_ReconcilerStore_PruneProcessedEvents_CtxCancelled(t *testing.T) {
	t.Parallel()
	_, _, sysPool := setupTestDB(t)
	ctx := context.Background()

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	store := pgadapter.NewReconcilerStore(sysPool)
	_, err := store.PruneProcessedEvents(cancelCtx, 8, 100)
	assert.Error(t, err)
}

// ═══════════════════════════════════════════════════════════════════════════
// IdempotencyRepository — error paths
// ═══════════════════════════════════════════════════════════════════════════

// Test Case ID:      ERRPATH-IDEMP-001
// Feature:           IdempotencyRepository.IsProcessed — context cancelled
// Lines:             idempotency_repository.go:31,37
// Technique:         Cancel ctx → QueryRow fails → return false, err
func TestErrPath_Idempotency_IsProcessed_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, _, _ := setupTestDB(t)
	ctx := context.Background()

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	repo := pgadapter.NewIdempotencyRepository(appPool)
	result, err := repo.IsProcessed(cancelCtx, "test-consumer", "test-event-id")
	assert.False(t, result)
	assert.Error(t, err, "cancelled context must propagate as error from IsProcessed")
}

// Test Case ID:      ERRPATH-IDEMP-002
// Feature:           IdempotencyRepository.MarkProcessed — context cancelled
// Lines:             idempotency_repository.go:47
// Technique:         Cancel ctx → Exec fails
func TestErrPath_Idempotency_MarkProcessed_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, _, _ := setupTestDB(t)
	ctx := context.Background()

	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	repo := pgadapter.NewIdempotencyRepository(appPool)
	err := repo.MarkProcessed(cancelCtx, "test-consumer", "test-event-id")
	assert.Error(t, err)
}

// ═══════════════════════════════════════════════════════════════════════════
// AuthZRepository — error paths
// ═══════════════════════════════════════════════════════════════════════════

// Test Case ID:      ERRPATH-AUTHZ-001
// Feature:           AuthZRepository.FindMembershipProjection — no rows → nil (not an error)
// Lines:             authz_repository.go:52,53,54,56
// Technique:         Non-existent user ID → ErrNoRows → out stays nil, nil error
func TestErrPath_AuthZ_FindMembershipProjection_NotFound(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "authz-notfound")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewAuthZRepository(appPool)
	proj, err := repo.FindMembershipProjection(tctx, tenantID, uuid.New())
	require.NoError(t, err, "not-found in AuthZ must return nil, nil (caller maps to 404)")
	assert.Nil(t, proj)
}

// Test Case ID:      ERRPATH-AUTHZ-002
// Feature:           AuthZRepository.FindMembershipProjection — context cancelled
// Lines:             authz_repository.go:44,56
// Technique:         Cancel ctx → first QueryRow fails
func TestErrPath_AuthZ_FindMembershipProjection_CtxCancelled(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "authz-ctx")
	tctx := withTenant(ctx, tenantID)

	cancelCtx, cancel := context.WithCancel(tctx)
	cancel()

	repo := pgadapter.NewAuthZRepository(appPool)
	_, err := repo.FindMembershipProjection(cancelCtx, tenantID, uuid.New())
	assert.Error(t, err)
}

// Test Case ID:      ERRPATH-AUTHZ-003
// Feature:           AuthZRepository.FindMembershipProjection — tenant roles query path
// Lines:             authz_repository.go:59,64,70,77,86,92,100,106,124
// Technique:         Full happy path with data → exercises all scan branches including roles+depts
func TestErrPath_AuthZ_FindMembershipProjection_WithRolesAndDepts(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "authz-with-roles")
	userID := uuid.New()
	memID := uuid.New()
	deptID := uuid.New()
	tctx := withTenant(ctx, tenantID)

	// Seed membership
	_, err := rawPool.Exec(ctx,
		`INSERT INTO tenant_memberships (id, tenant_id, user_id, status) VALUES ($1, $2, $3, 'active')`,
		memID, tenantID, userID)
	require.NoError(t, err)

	// Seed tenant role (must include tenant_membership_id — non-null FK)
	_, err = rawPool.Exec(ctx,
		`INSERT INTO tenant_roles (id, tenant_id, user_id, tenant_membership_id, role_code, granted_by) VALUES (gen_random_uuid(), $1, $2, $3, 'tenant_admin', $2)`,
		tenantID, userID, memID)
	require.NoError(t, err)

	// Seed dept membership
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
}

// Test Case ID:      ERRPATH-AUTHZ-004
// Feature:           AuthZRepository.FindMembershipProjection — member exists, no roles, no depts
// Lines:             authz_repository.go:67,68,77,86,100,106,124
// Technique:         Member row exists but tenant_roles empty and dept_memberships empty → empty slices
func TestErrPath_AuthZ_FindMembershipProjection_NoRolesNoDepts(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
}
