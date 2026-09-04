package postgres

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

// tdRow builds a scripted row matching scanTenantDept's Scan order.
func tdRow(tenantID, deptID uuid.UUID, isActive bool, rv int64) []any {
	now := time.Now()
	return []any{tenantID, deptID, isActive, rv, now, now}
}

// ── SetActive (P-25 optimistic-locked flip) ────────────────────────────

func TestTenantDepartmentRepo_SetActive_SuccessOnMatch(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: tdRow(tenantID, deptID, false, 2)}
		},
	}
	got, err := NewTenantDepartmentRepository(nil).
		SetActive(injectTx(context.Background(), tx), tenantID, deptID, false, 1)
	require.NoError(t, err)
	assert.False(t, got.IsActive)
}

func TestTenantDepartmentRepo_SetActive_NoRowsProbeConflictWhenVersionDiffers(t *testing.T) {
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{err: pgx.ErrNoRows}
			}
			return &fakeRow{values: []any{int64(7)}}
		},
	}
	_, err := NewTenantDepartmentRepository(nil).
		SetActive(injectTx(context.Background(), tx), uuid.New(), uuid.New(), true, 1)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
	assert.EqualValues(t, 7, de.Details["record_version"])
}

func TestTenantDepartmentRepo_SetActive_ProbeMissingMapsToDepartmentNotFound(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := NewTenantDepartmentRepository(nil).
		SetActive(injectTx(context.Background(), tx), uuid.New(), uuid.New(), true, 1)
	assert.ErrorIs(t, err, domain.ErrDepartmentNotFound)
}

func TestTenantDepartmentRepo_SetActive_ProbeUnrecognizedErrorPassesThrough(t *testing.T) {
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
	_, err := NewTenantDepartmentRepository(nil).
		SetActive(injectTx(context.Background(), tx), uuid.New(), uuid.New(), true, 1)
	assert.ErrorIs(t, err, probeErr)
}

func TestTenantDepartmentRepo_SetActive_UnrecognizedUpdateErrorPassesThrough(t *testing.T) {
	updateErr := errors.New("update failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: updateErr}
		},
	}
	_, err := NewTenantDepartmentRepository(nil).
		SetActive(injectTx(context.Background(), tx), uuid.New(), uuid.New(), true, 1)
	assert.ErrorIs(t, err, updateErr)
}

// ── Activate ─────────────────────────────────────────────────────────

func TestTenantDepartmentRepo_Activate_SuccessOnFreshRow(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: tdRow(tenantID, deptID, true, 1)}
		},
	}
	got, err := NewTenantDepartmentRepository(nil).
		Activate(injectTx(context.Background(), tx), tenantID, deptID)
	require.NoError(t, err)
	assert.True(t, got.IsActive)
}

func TestTenantDepartmentRepo_Activate_NoRowsMapsToAlreadyActivated(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := NewTenantDepartmentRepository(nil).
		Activate(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrDepartmentAlreadyActivated)
}

// ── Find ─────────────────────────────────────────────────────────────

func TestTenantDepartmentRepo_Find_NoRowsMapsToDepartmentNotFound(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := NewTenantDepartmentRepository(nil).
		Find(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrDepartmentNotFound)
}

func TestTenantDepartmentRepo_Find_ReturnsRow(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: tdRow(tenantID, deptID, true, 1)}
		},
	}
	got, err := NewTenantDepartmentRepository(nil).
		Find(injectTx(context.Background(), tx), tenantID, deptID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.IsActive)
}

func TestTenantDepartmentRepo_Find_UnrecognizedErrorPassesThrough(t *testing.T) {
	findErr := errors.New("find failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: findErr}
		},
	}
	_, err := NewTenantDepartmentRepository(nil).
		Find(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, findErr)
}

// ── Activate error path ─────────────────────────────────────────────────

func TestTenantDepartmentRepo_Activate_UnrecognizedErrorPassesThrough(t *testing.T) {
	insertErr := errors.New("insert failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: insertErr}
		},
	}
	_, err := NewTenantDepartmentRepository(nil).
		Activate(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, insertErr)
}

// ── listWhere error paths (via List/ListActive) ─────────────────────────

func TestTenantDepartmentRepo_List_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewTenantDepartmentRepository(nil).List(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, queryErr)
}

func TestTenantDepartmentRepo_List_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil
		},
	}
	_, err := NewTenantDepartmentRepository(nil).List(injectTx(context.Background(), tx), uuid.New())
	assert.Error(t, err)
}

func TestTenantDepartmentRepo_List_IterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("iteration failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewTenantDepartmentRepository(nil).List(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, iterErr)
}

func TestTenantDepartmentRepo_ListActive_AppendsActiveFilter(t *testing.T) {
	tenantID := uuid.New()
	var gotSQL string
	tx := &fakeTx{
		queryFn: func(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
			gotSQL = sql
			return &fakeRows{scripted: [][]any{tdRow(tenantID, uuid.New(), true, 1)}}, nil
		},
	}
	got, err := NewTenantDepartmentRepository(nil).ListActive(injectTx(context.Background(), tx), tenantID)
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Contains(t, gotSQL, "is_active = true")
}
