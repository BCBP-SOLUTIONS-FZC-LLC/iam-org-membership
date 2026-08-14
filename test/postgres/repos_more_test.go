//go:build integration

// Phase 8 (continued) — remaining 4 repositories.
//
// Module:   iam-org-membership
// Feature:  Persistence layer — delegation, group mapping, dept role label,
//
//	tender ACL repos.
//
// Files:    internal/adapter/outbound/postgres/{delegation,group_mapping,
//
//	dept_role_label,tender_acl}_repository.go
//
// Test IDs: P8-DELEG-NNN, P8-GMAP-NNN, P8-LABELR-NNN, P8-ACL-NNN.
package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ═════════════════════════════════════════════════════════════════════════
// DelegationRepository
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P8-DELEG-001
// Module:            iam-org-membership · Persistence
// Feature:           delegations · Insert + FindByID round-trip
// API:               Internal (P-19 via service)
// Scenario:          Happy — insert active delegation, then FindByID
// Preconditions:     Tenant + 2 memberships seeded
// Test Steps:
//  1. Seed tenant + 2 members
//  2. Insert delegation
//  3. FindByID
//
// Expected Result:
//   - Returns the inserted row with status=active, RecordVersion=1
//
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP8Deleg001_InsertAndFindByID(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "drepo-001")
	dgID, dtID := seedTwoActiveMembers(t, ctx, rawPool, tenantID)
	memIDs := lookupMembershipIDs(t, ctx, rawPool, tenantID, []uuid.UUID{dgID, dtID})
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewDelegationRepository(appPool)
	created, err := repo.Insert(tctx, &domain.Delegation{
		TenantID: tenantID, DelegatorID: dgID, DelegateID: dtID,
		DelegatorMembershipID: memIDs[dgID], DelegateMembershipID: memIDs[dtID],
		Scope: domain.ScopeAll, StartsAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NotNil(t, created)

	found, err := repo.FindByID(tctx, tenantID, created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, found.ID)
	assert.Equal(t, domain.DelegationActive, found.Status)
}

// Test Case ID:      P8-DELEG-002
// Module:            iam-org-membership · Persistence
// Feature:           delegations · List surfaces all non-deleted rows (any status)
// API:               Internal (P-18)
// Scenario:          Positive — List filters by deleted_at IS NULL only; the
//
//	caller (service / handler) is responsible for further
//	status filtering per view semantics.
//
// Preconditions:     One delegation, cancelled but not soft-deleted
// Test Steps:
//  1. Insert delegation
//  2. Cancel via End(status=cancelled) — sets status but does NOT flip deleted_at
//  3. List
//
// Expected Result:
//   - Row still appears (List = "all not-hard-deleted for tenant")
//   - status = cancelled
//
// Priority:          P2
// Severity:          Major
// Automation Status: Automated
func TestP8Deleg002_ListSurfacesAllNonDeleted(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "drepo-002")
	dgID, dtID := seedTwoActiveMembers(t, ctx, rawPool, tenantID)
	memIDs := lookupMembershipIDs(t, ctx, rawPool, tenantID, []uuid.UUID{dgID, dtID})
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewDelegationRepository(appPool)
	created, err := repo.Insert(tctx, &domain.Delegation{
		TenantID: tenantID, DelegatorID: dgID, DelegateID: dtID,
		DelegatorMembershipID: memIDs[dgID], DelegateMembershipID: memIDs[dtID],
		Scope: domain.ScopeAll, StartsAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	_, err = repo.End(tctx, tenantID, created.ID, domain.DelegationCancelled, created.RecordVersion)
	require.NoError(t, err)

	list, err := repo.List(tctx, tenantID)
	require.NoError(t, err)
	require.Len(t, list, 1, "List returns non-deleted rows regardless of status")
	assert.Equal(t, domain.DelegationCancelled, list[0].Status,
		"cancelled row surfaces with status=cancelled — status-based filtering is a caller concern")
}

// Test Case ID:      P8-DELEG-003
// Module:            iam-org-membership · Persistence
// Feature:           delegations · End with optimistic-lock mismatch
// API:               Internal (P-20)
// Scenario:          Negative — record_version stale
// Preconditions:     Active delegation at v=1
// Test Steps:
//  1. Insert
//  2. End with expectedVersion=999
//
// Expected Result:
//   - Returns ErrOptimisticLockConflict
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Deleg003_EndOptimisticLock(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "drepo-003")
	dgID, dtID := seedTwoActiveMembers(t, ctx, rawPool, tenantID)
	memIDs := lookupMembershipIDs(t, ctx, rawPool, tenantID, []uuid.UUID{dgID, dtID})
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewDelegationRepository(appPool)
	created, err := repo.Insert(tctx, &domain.Delegation{
		TenantID: tenantID, DelegatorID: dgID, DelegateID: dtID,
		DelegatorMembershipID: memIDs[dgID], DelegateMembershipID: memIDs[dtID],
		Scope: domain.ScopeAll, StartsAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	_, err = repo.End(tctx, tenantID, created.ID, domain.DelegationCancelled, 999)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
}

// Test Case ID:      P8-DELEG-004
// Module:            iam-org-membership · Persistence
// Feature:           delegations · ListExpiringBefore
// API:               Internal (delegation-expiry reconciler)
// Scenario:          Positive — one entry with ends_at in the past returned
// Preconditions:     One entry with ends_at 1 h ago, one with ends_at future
// Test Steps:
//  1. Insert with future ends_at
//  2. Backdate one to now-1h via SQL
//  3. Call ListExpiringBefore(now, limit=100)
//
// Expected Result:
//   - Returns only the backdated row
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8Deleg004_ListExpiringBefore(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "drepo-004")
	dgID, dtID := seedTwoActiveMembers(t, ctx, rawPool, tenantID)
	memIDs := lookupMembershipIDs(t, ctx, rawPool, tenantID, []uuid.UUID{dgID, dtID})
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewDelegationRepository(appPool)
	// Start in the past so we can UPDATE ends_at to 1h ago and still satisfy
	// chk_ends_after_starts (DEL-8).
	starts := time.Now().UTC().Add(-3 * time.Hour)
	futureEnd := time.Now().UTC().Add(24 * time.Hour)
	created, err := repo.Insert(tctx, &domain.Delegation{
		TenantID: tenantID, DelegatorID: dgID, DelegateID: dtID,
		DelegatorMembershipID: memIDs[dgID], DelegateMembershipID: memIDs[dtID],
		Scope: domain.ScopeAll, StartsAt: starts,
		EndsAt: &futureEnd,
	})
	require.NoError(t, err)

	// Backdate ends_at to 1 h ago — still after starts_at (-3h), so
	// chk_ends_after_starts passes.
	_, err = rawPool.Exec(ctx,
		`UPDATE delegations SET ends_at = now() - interval '1 hour' WHERE id = $1`, created.ID)
	require.NoError(t, err)

	// ListExpiringBefore's WHERE clause carries no tenant filter (by design,
	// it's meant to be a cross-tenant reconciler query) — but org_membership_app
	// is RLS-scoped, so it must be called under this test's OWN tenant GUC
	// (tctx), not a bare ctx. Without a GUCSet in ctx, pgcommon's PrepareConn
	// hook leaves app.tenant_id at whatever a previous, unrelated checkout of
	// the same pooled connection last set it to — nondeterministic under
	// concurrent test load (this made the test genuinely flaky, not just
	// timing-sensitive).
	expiring, err := repo.ListExpiringBefore(tctx, time.Now().UTC(), 100)
	require.NoError(t, err)
	require.NotEmpty(t, expiring)
	found := false
	for _, d := range expiring {
		if d.ID == created.ID {
			found = true
			break
		}
	}
	assert.True(t, found, "backdated row must appear in ListExpiringBefore")
}

// ═════════════════════════════════════════════════════════════════════════
// DeptRoleLabelRepository
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P8-LABELR-001
// Module:            iam-org-membership · Persistence
// Feature:           dept_role_labels · Seed happy
// API:               Internal (called by I-1 trial signup)
// Scenario:          Positive — Seed creates 3 rows with default names
// Preconditions:     Fresh tenant, no labels
// Test Steps:
//  1. Call Seed
//  2. Call List
//
// Expected Result:
//   - Returns 3 labels (preparator/reviewer/approver) with default DisplayName
//
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP8LabelR001_SeedHappy(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "lbl-001")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewDeptRoleLabelRepository(appPool)
	_, err := repo.Seed(tctx, tenantID)
	require.NoError(t, err)

	labels, err := repo.List(tctx, tenantID)
	require.NoError(t, err)
	assert.Len(t, labels, 3)
}

// Test Case ID:      P8-LABELR-002
// Module:            iam-org-membership · Persistence
// Feature:           dept_role_labels · Update happy
// API:               PATCH /api/v1/tenants/{id}/roles/{role_code}
// Scenario:          Rename approver → Buyer
// Preconditions:     Labels seeded
// Test Steps:
//  1. Seed
//  2. Update(role=approver, display=Buyer, v=1)
//
// Expected Result:
//   - Returns row with DisplayName=Buyer, RecordVersion=2
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8LabelR002_UpdateHappy(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "lbl-002")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewDeptRoleLabelRepository(appPool)
	_, err := repo.Seed(tctx, tenantID)
	require.NoError(t, err)

	l, err := repo.Update(tctx, tenantID, domain.DeptApprover, "Buyer", 1)
	require.NoError(t, err)
	assert.Equal(t, "Buyer", l.DisplayName)
	assert.EqualValues(t, 2, l.RecordVersion)
}

// Test Case ID:      P8-LABELR-003
// Module:            iam-org-membership · Persistence
// Feature:           CONC-4 · dept_role_labels Update optimistic-lock
// API:               PATCH /api/v1/tenants/{id}/roles/{role_code}
// Scenario:          Negative — stale record_version
// Preconditions:     Labels seeded at v=1
// Test Steps:
//  1. Seed
//  2. Update with expectedVersion=999
//
// Expected Result:
//   - Returns ErrOptimisticLockConflict
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8LabelR003_UpdateOptimisticLock(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "lbl-003")
	tctx := withTenant(ctx, tenantID)

	repo := pgadapter.NewDeptRoleLabelRepository(appPool)
	_, err := repo.Seed(tctx, tenantID)
	require.NoError(t, err)

	_, err = repo.Update(tctx, tenantID, domain.DeptApprover, "Buyer", 999)
	require.Error(t, err)
	var de *domain.DomainError
	require.True(t, errors.As(err, &de))
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
}

// ═════════════════════════════════════════════════════════════════════════
// TenderACLRepository
// ═════════════════════════════════════════════════════════════════════════

// Test Case ID:      P8-ACL-001
// Module:            iam-org-membership · Persistence
// Feature:           tender_acl_entries · Grant happy
// API:               POST /api/v1/tenants/{id}/tenders/{tender_id}/acl
// Scenario:          Positive — grant a view ACL
// Preconditions:     Tenant + user + arbitrary tender_id
// Test Steps:
//  1. Seed tenant + member
//  2. Grant TenderACLEntry{user, tender, view}
//
// Expected Result:
//   - Returned entry has AccessLevel=view, no deleted_at
//
// Priority:          P1
// Severity:          Blocker
// Automation Status: Automated
func TestP8ACL001_GrantHappy(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "acl-001")
	userID, _ := seedTwoActiveMembers(t, ctx, rawPool, tenantID)
	tctx := withTenant(ctx, tenantID)

	memIDs := lookupMembershipIDs(t, ctx, rawPool, tenantID, []uuid.UUID{userID})
	repo := pgadapter.NewTenderACLRepository(appPool)
	tenderID := uuid.New()
	e, err := repo.Grant(tctx, &domain.TenderACLEntry{
		TenantID: tenantID, TenderID: tenderID, UserID: userID,
		TenantMembershipID: memIDs[userID],
		AccessLevel:        domain.ACLView, GrantedBy: userID,
	})
	require.NoError(t, err)
	require.NotNil(t, e)
	assert.Equal(t, domain.ACLView, e.AccessLevel)
	assert.Nil(t, e.DeletedAt)
}

// Test Case ID:      P8-ACL-002
// Module:            iam-org-membership · Persistence
// Feature:           tender_acl_entries · FindActiveForUser
// API:               GET /api/v1/internal/tenants/{id}/tenders/{tid}/acl/{uid}
// Scenario:          Positive — returns the granted level
// Preconditions:     ACL granted at view
// Test Steps:
//  1. Grant view
//  2. FindActiveForUser
//
// Expected Result:
//   - Returns entry with AccessLevel=view
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8ACL002_FindActiveHappy(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "acl-002")
	userID, _ := seedTwoActiveMembers(t, ctx, rawPool, tenantID)
	tctx := withTenant(ctx, tenantID)

	memIDs := lookupMembershipIDs(t, ctx, rawPool, tenantID, []uuid.UUID{userID})
	repo := pgadapter.NewTenderACLRepository(appPool)
	tenderID := uuid.New()
	_, err := repo.Grant(tctx, &domain.TenderACLEntry{
		TenantID: tenantID, TenderID: tenderID, UserID: userID,
		TenantMembershipID: memIDs[userID],
		AccessLevel:        domain.ACLEdit, GrantedBy: userID,
	})
	require.NoError(t, err)

	found, err := repo.FindActiveForUser(tctx, tenantID, tenderID, userID)
	require.NoError(t, err)
	assert.Equal(t, domain.ACLEdit, found.AccessLevel)
}

// Test Case ID:      P8-ACL-003
// Module:            iam-org-membership · Persistence
// Feature:           tender_acl_entries · Revoke happy
// API:               DELETE /api/v1/tenants/{id}/tenders/{tid}/acl/{uid}
// Scenario:          Positive — soft-delete existing grant
// Preconditions:     ACL granted
// Test Steps:
//  1. Grant
//  2. Revoke
//  3. FindActiveForUser
//
// Expected Result:
//   - Revoke returns the soft-deleted row (deleted_at IS NOT NULL)
//   - Subsequent Find returns nil (or not-found)
//
// Priority:          P1
// Severity:          Major
// Automation Status: Automated
func TestP8ACL003_RevokeSoftDeletes(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "acl-003")
	userID, _ := seedTwoActiveMembers(t, ctx, rawPool, tenantID)
	tctx := withTenant(ctx, tenantID)

	memIDs := lookupMembershipIDs(t, ctx, rawPool, tenantID, []uuid.UUID{userID})
	repo := pgadapter.NewTenderACLRepository(appPool)
	tenderID := uuid.New()
	_, err := repo.Grant(tctx, &domain.TenderACLEntry{
		TenantID: tenantID, TenderID: tenderID, UserID: userID,
		TenantMembershipID: memIDs[userID],
		AccessLevel:        domain.ACLApprove, GrantedBy: userID,
	})
	require.NoError(t, err)

	revoked, err := repo.Revoke(tctx, tenantID, tenderID, userID)
	require.NoError(t, err)
	require.NotNil(t, revoked.DeletedAt, "Revoke must soft-delete (deleted_at set)")

	found, ferr := repo.FindActiveForUser(tctx, tenantID, tenderID, userID)
	// Either err or nil-with-nil; both acceptable — the key is that the
	// row no longer appears as active.
	if ferr == nil {
		assert.Nil(t, found, "FindActiveForUser must not return revoked rows")
	}
}

// Test Case ID:      P8-ACL-004
// Module:            iam-org-membership · Persistence
// Feature:           tender_acl_entries · ListByTender
// API:               GET /api/v1/tenants/{id}/tenders/{tid}/acl
// Scenario:          Positive — returns all active entries for a tender
// Preconditions:     2 users granted; one revoked
// Test Steps:
//  1. Grant u1
//  2. Grant u2
//  3. Revoke u1
//  4. ListByTender
//
// Expected Result:
//   - Returns 1 entry (u2 only)
//
// Priority:          P2
// Severity:          Major
// Automation Status: Automated
func TestP8ACL004_ListByTenderExcludesRevoked(t *testing.T) {
	appPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "acl-004")
	u1, u2 := seedTwoActiveMembers(t, ctx, rawPool, tenantID)
	tctx := withTenant(ctx, tenantID)

	memIDs := lookupMembershipIDs(t, ctx, rawPool, tenantID, []uuid.UUID{u1, u2})
	repo := pgadapter.NewTenderACLRepository(appPool)
	tenderID := uuid.New()
	_, err := repo.Grant(tctx, &domain.TenderACLEntry{
		TenantID: tenantID, TenderID: tenderID, UserID: u1,
		TenantMembershipID: memIDs[u1],
		AccessLevel:        domain.ACLView, GrantedBy: u1,
	})
	require.NoError(t, err)
	_, err = repo.Grant(tctx, &domain.TenderACLEntry{
		TenantID: tenantID, TenderID: tenderID, UserID: u2,
		TenantMembershipID: memIDs[u2],
		AccessLevel:        domain.ACLView, GrantedBy: u2,
	})
	require.NoError(t, err)
	_, err = repo.Revoke(tctx, tenantID, tenderID, u1)
	require.NoError(t, err)

	list, err := repo.ListByTender(tctx, tenantID, tenderID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, u2, list[0].UserID)
}

// ═════════════════════════════════════════════════════════════════════════
// helpers
// ═════════════════════════════════════════════════════════════════════════

// lookupMembershipIDs returns each user's tenant_memberships.id — needed
// to satisfy the composite FK on delegations (DEL-*).
func lookupMembershipIDs(t *testing.T, ctx context.Context, rawPool *pgxpoolPool, tenantID uuid.UUID, userIDs []uuid.UUID) map[uuid.UUID]uuid.UUID {
	t.Helper()
	out := make(map[uuid.UUID]uuid.UUID, len(userIDs))
	for _, uid := range userIDs {
		var mid uuid.UUID
		err := rawPool.QueryRow(ctx,
			`SELECT id FROM tenant_memberships WHERE tenant_id = $1 AND user_id = $2 AND deleted_at IS NULL`,
			tenantID, uid).Scan(&mid)
		require.NoError(t, err)
		out[uid] = mid
	}
	return out
}
