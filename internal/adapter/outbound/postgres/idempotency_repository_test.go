package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── IsProcessed ──────────────────────────────────────────────────────────

func TestIdempotencyRepo_IsProcessed_TrueWhenRowExists(t *testing.T) {
	var gotSQL string
	var gotArgs []any
	tx := &fakeTx{
		queryRowFn: func(_ context.Context, sql string, args ...any) pgx.Row {
			gotSQL = sql
			gotArgs = args
			return &fakeRow{values: []any{true}}
		},
	}
	repo := NewIdempotencyRepository(nil)
	got, err := repo.IsProcessed(injectTx(context.Background(), tx), "membership-consumer", "evt-1")
	require.NoError(t, err)
	assert.True(t, got)
	assert.Contains(t, gotSQL, "SELECT EXISTS")
	require.Len(t, gotArgs, 2)
	assert.Equal(t, "evt-1", gotArgs[0])
	assert.Equal(t, "membership-consumer", gotArgs[1])
}

func TestIdempotencyRepo_IsProcessed_FalseWhenRowMissing(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: []any{false}}
		},
	}
	repo := NewIdempotencyRepository(nil)
	got, err := repo.IsProcessed(injectTx(context.Background(), tx), "billing-consumer", "evt-2")
	require.NoError(t, err)
	assert.False(t, got)
}

func TestIdempotencyRepo_IsProcessed_ScanErrorReturnsFalseAndError(t *testing.T) {
	scanErr := errors.New("scan failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: scanErr}
		},
	}
	repo := NewIdempotencyRepository(nil)
	got, err := repo.IsProcessed(injectTx(context.Background(), tx), "consumer", "evt-3")
	assert.ErrorIs(t, err, scanErr)
	assert.False(t, got)
}

// ── MarkProcessed ────────────────────────────────────────────────────────

func TestIdempotencyRepo_MarkProcessed_InsertsWithOnConflictDoNothing(t *testing.T) {
	var gotSQL string
	var gotArgs []any
	tx := &fakeTx{
		execFn: func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
			gotSQL = sql
			gotArgs = args
			return pgconn.NewCommandTag("INSERT 0 1"), nil
		},
	}
	repo := NewIdempotencyRepository(nil)
	err := repo.MarkProcessed(injectTx(context.Background(), tx), "membership-consumer", "evt-4")
	require.NoError(t, err)
	assert.Contains(t, gotSQL, "ON CONFLICT (event_id, consumer) DO NOTHING")
	require.Len(t, gotArgs, 2)
	assert.Equal(t, "evt-4", gotArgs[0])
	assert.Equal(t, "membership-consumer", gotArgs[1])
}

func TestIdempotencyRepo_MarkProcessed_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("insert failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	repo := NewIdempotencyRepository(nil)
	err := repo.MarkProcessed(injectTx(context.Background(), tx), "consumer", "evt-5")
	assert.ErrorIs(t, err, execErr)
}
