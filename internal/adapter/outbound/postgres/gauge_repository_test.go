package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gauge_repository.go has no port interface — these are whitebox tests
// against the concrete GaugeRepository, exercising the shared count()
// helper via each of its four public entry points plus the error path.

func TestGaugeRepository_CountOwnerlessTenants_ReturnsScannedValue(t *testing.T) {
	var gotSQL string
	tx := &fakeTx{
		queryRowFn: func(_ context.Context, sql string, _ ...any) pgx.Row {
			gotSQL = sql
			return &fakeRow{values: []any{int64(3)}}
		},
	}
	repo := NewGaugeRepository(nil)
	n, err := repo.CountOwnerlessTenants(injectTx(context.Background(), tx))
	require.NoError(t, err)
	assert.EqualValues(t, 3, n)
	assert.Contains(t, gotSQL, "ownerless_since IS NOT NULL")
}

func TestGaugeRepository_CountRealmSyncPending_ReturnsScannedValue(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: []any{int64(1)}}
		},
	}
	repo := NewGaugeRepository(nil)
	n, err := repo.CountRealmSyncPending(injectTx(context.Background(), tx))
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
}

func TestGaugeRepository_CountSeatOverageActive_ReturnsScannedValue(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: []any{int64(0)}}
		},
	}
	repo := NewGaugeRepository(nil)
	n, err := repo.CountSeatOverageActive(injectTx(context.Background(), tx))
	require.NoError(t, err)
	assert.EqualValues(t, 0, n)
}

func TestGaugeRepository_CountPendingInvitationsStale_ReturnsScannedValue(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: []any{int64(5)}}
		},
	}
	repo := NewGaugeRepository(nil)
	n, err := repo.CountPendingInvitationsStale(injectTx(context.Background(), tx))
	require.NoError(t, err)
	assert.EqualValues(t, 5, n)
}

// count's error path (shared by all four callers) must return 0, not a
// stale partial value, and pass the underlying error through unchanged.
func TestGaugeRepository_count_ScanErrorReturnsZeroAndError(t *testing.T) {
	scanErr := errors.New("scan failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: scanErr}
		},
	}
	repo := NewGaugeRepository(nil)
	n, err := repo.CountOwnerlessTenants(injectTx(context.Background(), tx))
	assert.ErrorIs(t, err, scanErr)
	assert.EqualValues(t, 0, n)
}
