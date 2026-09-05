package postgres

// tenant_repository_optimistic_test.go — unit tests for
// TenantRepository.optimisticConflictOrNotFound.
//
// Three branches:
//  1. row.Scan returns pgx.ErrNoRows → ErrTenantNotFound (404, genuine miss)
//  2. row.Scan returns another error → that error is returned verbatim
//  3. row.Scan succeeds (tenant exists, version mismatch) →
//     ErrOptimisticLockConflict with record_version / updated_at detail
//
// The function takes a pgx.Tx directly, so we inject a fakeTx (from
// fakes_test.go, same package) whose QueryRow is scripted per scenario.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOptimisticConflictOrNotFound_TenantNotFound verifies that when the probe
// query returns pgx.ErrNoRows (tenant deleted or never existed), the function
// surfaces domain.ErrTenantNotFound.
func TestOptimisticConflictOrNotFound_TenantNotFound(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	repo := NewTenantRepository(nil)
	err := repo.optimisticConflictOrNotFound(context.Background(), tx, uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrTenantNotFound,
		"pgx.ErrNoRows from the probe must map to ErrTenantNotFound (CONC-3 §16 A7)")
}

// TestOptimisticConflictOrNotFound_OtherScanError verifies that an unexpected
// scan error (e.g. a type mismatch or network failure) is returned verbatim
// rather than masked as a 404 or 409.
func TestOptimisticConflictOrNotFound_OtherScanError(t *testing.T) {
	sentinel := errors.New("unexpected scan failure")
	tx := &fakeTx{
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			return &fakeRow{err: sentinel}
		},
	}
	repo := NewTenantRepository(nil)
	err := repo.optimisticConflictOrNotFound(context.Background(), tx, uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel,
		"non-ErrNoRows scan errors must be returned verbatim, not wrapped as 409/404")
}

// TestOptimisticConflictOrNotFound_VersionConflict verifies that when the
// probe row returns successfully (tenant found, current version/timestamp),
// the function returns ErrOptimisticLockConflict with a WithDetails payload
// containing record_version and updated_at (CONC-1..4, §16 A7).
func TestOptimisticConflictOrNotFound_VersionConflict(t *testing.T) {
	expectedVersion := int64(7)
	expectedUpdatedAt := time.Now().UTC().Truncate(time.Second)

	tx := &fakeTx{
		queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
			// Scan populates (record_version, updated_at) in that order.
			return &fakeRow{values: []any{expectedVersion, expectedUpdatedAt}}
		},
	}
	repo := NewTenantRepository(nil)
	err := repo.optimisticConflictOrNotFound(context.Background(), tx, uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrOptimisticLockConflict,
		"tenant found → must be a version conflict, not a 404")

	// Also verify the details map is attached — callers use it to echo back
	// the current version in a 409 response body.
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	require.NotNil(t, de.Details, "ErrOptimisticLockConflict must carry WithDetails (CONC-3)")
	assert.Equal(t, expectedVersion, de.Details["record_version"],
		"record_version in details must match the probed row")
}
