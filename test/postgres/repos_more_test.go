//go:build integration

// Phase 8 (continued) — remaining repositories.
//
// Module:   iam-org-membership
// Feature:  Persistence layer — group mapping, dept role label repos.
//
//	(tender ACL repo tests retired ADR-0007 Wave 3 Phase 6, moved to
//	iam-tender-acl; delegation repo tests retired ADR-0008 v2, moved to
//	the standalone Delegation Service.)
//
// Files:    internal/adapter/outbound/postgres/{group_mapping,
//
//	dept_role_label}_repository.go
//
// Test IDs: P8-GMAP-NNN, P8-LABELR-NNN.
package postgres_test

import (
	"context"
	"errors"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
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

// TenderACLRepository tests (P8-ACL-001..004) — retired ADR-0007 Wave 3
// Phase 6, moved to iam-tender-acl's repository test suite.

