package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── ListSeatOverageCandidates ────────────────────────────────────────────

func TestReconcilerStore_ListSeatOverageCandidates_ReturnsIDs(t *testing.T) {
	id1, id2 := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{id1}, {id2}}}, nil
		},
	}
	got, err := NewReconcilerStore(nil).
		ListSeatOverageCandidates(injectTx(context.Background(), tx), 10)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{id1, id2}, got)
}

func TestReconcilerStore_ListSeatOverageCandidates_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewReconcilerStore(nil).
		ListSeatOverageCandidates(injectTx(context.Background(), tx), 10)
	assert.ErrorIs(t, err, queryErr)
}

func TestReconcilerStore_ListSeatOverageCandidates_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			// two dest columns expected (just id) but scripted gives a value
			// that fails scanInto's pointer-type check (a plain string won't
			// convert to uuid.UUID).
			return &fakeRows{scripted: [][]any{{"not-a-uuid"}}}, nil
		},
	}
	_, err := NewReconcilerStore(nil).
		ListSeatOverageCandidates(injectTx(context.Background(), tx), 10)
	assert.Error(t, err)
}

func TestReconcilerStore_ListSeatOverageCandidates_IterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("iteration failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewReconcilerStore(nil).
		ListSeatOverageCandidates(injectTx(context.Background(), tx), 10)
	assert.ErrorIs(t, err, iterErr)
}

// ── ListRealmSyncPending ─────────────────────────────────────────────────

func TestReconcilerStore_ListRealmSyncPending_ReturnsCandidates(t *testing.T) {
	id1 := uuid.New()
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{id1, true}}}, nil
		},
	}
	got, err := NewReconcilerStore(nil).
		ListRealmSyncPending(injectTx(context.Background(), tx), 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, id1, got[0].TenantID)
	assert.True(t, got[0].LocalAccountsEnabled)
}

func TestReconcilerStore_ListRealmSyncPending_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewReconcilerStore(nil).
		ListRealmSyncPending(injectTx(context.Background(), tx), 10)
	assert.ErrorIs(t, err, queryErr)
}

func TestReconcilerStore_ListRealmSyncPending_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil // missing 2nd column
		},
	}
	_, err := NewReconcilerStore(nil).
		ListRealmSyncPending(injectTx(context.Background(), tx), 10)
	assert.Error(t, err)
}

func TestReconcilerStore_ListRealmSyncPending_IterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("iteration failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewReconcilerStore(nil).
		ListRealmSyncPending(injectTx(context.Background(), tx), 10)
	assert.ErrorIs(t, err, iterErr)
}

// ── ClearRealmSyncPending ────────────────────────────────────────────────

func TestReconcilerStore_ClearRealmSyncPending_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	err := NewReconcilerStore(nil).
		ClearRealmSyncPending(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, execErr)
}

func TestReconcilerStore_ClearRealmSyncPending_Success(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	err := NewReconcilerStore(nil).
		ClearRealmSyncPending(injectTx(context.Background(), tx), uuid.New())
	assert.NoError(t, err)
}

// ── HardDeleteExpiredTrials ──────────────────────────────────────────────

func TestReconcilerStore_HardDeleteExpiredTrials_ReturnsRowsAffected(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("DELETE 3"), nil
		},
	}
	n, err := NewReconcilerStore(nil).
		HardDeleteExpiredTrials(injectTx(context.Background(), tx), 30)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
}

func TestReconcilerStore_HardDeleteExpiredTrials_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("delete failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	_, err := NewReconcilerStore(nil).
		HardDeleteExpiredTrials(injectTx(context.Background(), tx), 30)
	assert.ErrorIs(t, err, execErr)
}

// PruneOutbox was removed 2026-09-20 — outbox pruning goes entirely through
// platform-events' outbox.Runner.PrunePublished now (cmd/reconciler/jobs/
// outbox_prune.go); this hand-rolled DELETE had zero production callers.

// ── PruneProcessedEvents ─────────────────────────────────────────────────

func TestReconcilerStore_PruneProcessedEvents_ReturnsRowsAffected(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("DELETE 2"), nil
		},
	}
	n, err := NewReconcilerStore(nil).
		PruneProcessedEvents(injectTx(context.Background(), tx), 8, 500)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestReconcilerStore_PruneProcessedEvents_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("delete failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	_, err := NewReconcilerStore(nil).
		PruneProcessedEvents(injectTx(context.Background(), tx), 8, 500)
	assert.ErrorIs(t, err, execErr)
}
