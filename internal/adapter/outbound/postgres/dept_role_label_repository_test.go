package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drlRow builds a scripted row matching scanDeptRoleLabel's Scan order.
func drlRow(tenantID uuid.UUID, roleCode string, displayName string, rv int64) []any {
	now := time.Now()
	return []any{uuid.New(), tenantID, roleCode, displayName, rv, now, now}
}

// ── List ─────────────────────────────────────────────────────────────

func TestDeptRoleLabelRepo_List_ReturnsAllLabels(t *testing.T) {
	tenantID := uuid.New()
	var gotSQL string
	tx := &fakeTx{
		queryFn: func(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
			gotSQL = sql
			return &fakeRows{scripted: [][]any{
				drlRow(tenantID, "preparator", "Preparator", 1),
				drlRow(tenantID, "reviewer", "Reviewer", 1),
			}}, nil
		},
	}
	got, err := NewDeptRoleLabelRepository(nil).List(injectTx(context.Background(), tx), tenantID)
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Contains(t, gotSQL, "ORDER BY role_code")
}

func TestDeptRoleLabelRepo_List_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewDeptRoleLabelRepository(nil).List(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, queryErr)
}

func TestDeptRoleLabelRepo_List_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil
		},
	}
	_, err := NewDeptRoleLabelRepository(nil).List(injectTx(context.Background(), tx), uuid.New())
	assert.Error(t, err)
}

func TestDeptRoleLabelRepo_List_IterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("iteration failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewDeptRoleLabelRepository(nil).List(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, iterErr)
}

// ── Update ───────────────────────────────────────────────────────────

func TestDeptRoleLabelRepo_Update_SuccessOnMatch(t *testing.T) {
	tenantID := uuid.New()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: drlRow(tenantID, "approver", "Buyer", 2)}
		},
	}
	got, err := NewDeptRoleLabelRepository(nil).
		Update(injectTx(context.Background(), tx), tenantID, domain.DeptApprover, "Buyer", 1)
	require.NoError(t, err)
	assert.Equal(t, "Buyer", got.DisplayName)
}

func TestDeptRoleLabelRepo_Update_NoRowsProbeConflictWhenVersionDiffers(t *testing.T) {
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{err: pgx.ErrNoRows}
			}
			return &fakeRow{values: []any{int64(5)}}
		},
	}
	_, err := NewDeptRoleLabelRepository(nil).
		Update(injectTx(context.Background(), tx), uuid.New(), domain.DeptApprover, "Buyer", 1)
	require.Error(t, err)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
	assert.EqualValues(t, 5, de.Details["record_version"])
}

func TestDeptRoleLabelRepo_Update_ProbeMissingMapsToMemberNotFound(t *testing.T) {
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := NewDeptRoleLabelRepository(nil).
		Update(injectTx(context.Background(), tx), uuid.New(), domain.DeptApprover, "Buyer", 1)
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
	assert.Equal(t, 2, callCount, "update-no-rows, then a not-found probe")
}

func TestDeptRoleLabelRepo_Update_ProbeUnrecognizedErrorPassesThrough(t *testing.T) {
	probeErr := errors.New("probe failed")
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{err: pgx.ErrNoRows}
			}
			return &fakeRow{err: probeErr}
		},
	}
	_, err := NewDeptRoleLabelRepository(nil).
		Update(injectTx(context.Background(), tx), uuid.New(), domain.DeptApprover, "Buyer", 1)
	assert.ErrorIs(t, err, probeErr)
}

func TestDeptRoleLabelRepo_Update_UnrecognizedUpdateErrorPassesThrough(t *testing.T) {
	updateErr := errors.New("update failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: updateErr}
		},
	}
	_, err := NewDeptRoleLabelRepository(nil).
		Update(injectTx(context.Background(), tx), uuid.New(), domain.DeptApprover, "Buyer", 1)
	assert.ErrorIs(t, err, updateErr)
}

// ── Seed ─────────────────────────────────────────────────────────────

func TestDeptRoleLabelRepo_Seed_InsertsThenListsLabels(t *testing.T) {
	tenantID := uuid.New()
	var execSQL string
	tx := &fakeTx{
		execFn: func(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
			execSQL = sql
			return pgconn.CommandTag{}, nil
		},
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{
				drlRow(tenantID, "preparator", "Preparator", 1),
				drlRow(tenantID, "reviewer", "Reviewer", 1),
				drlRow(tenantID, "approver", "Approver", 1),
			}}, nil
		},
	}
	got, err := NewDeptRoleLabelRepository(nil).Seed(injectTx(context.Background(), tx), tenantID)
	require.NoError(t, err)
	assert.Len(t, got, 3)
	assert.Contains(t, execSQL, "ON CONFLICT (tenant_id, role_code) DO NOTHING")
}

func TestDeptRoleLabelRepo_Seed_InsertErrorPassesThrough(t *testing.T) {
	insertErr := errors.New("insert failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, insertErr
		},
	}
	_, err := NewDeptRoleLabelRepository(nil).Seed(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, insertErr)
}
