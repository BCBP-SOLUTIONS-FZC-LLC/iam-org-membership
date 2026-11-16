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

// ── ListExpiredTrialTenants ──────────────────────────────────────────────

func TestReconcilerStore_ListExpiredTrialTenants_ReturnsIDs(t *testing.T) {
	id1, id2 := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{id1}, {id2}}}, nil
		},
	}
	got, err := NewReconcilerStore(nil).
		ListExpiredTrialTenants(injectTx(context.Background(), tx), 15, 500)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{id1, id2}, got)
}

func TestReconcilerStore_ListExpiredTrialTenants_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewReconcilerStore(nil).
		ListExpiredTrialTenants(injectTx(context.Background(), tx), 15, 500)
	assert.ErrorIs(t, err, queryErr)
}

func TestReconcilerStore_ListExpiredTrialTenants_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{"not-a-uuid"}}}, nil
		},
	}
	_, err := NewReconcilerStore(nil).
		ListExpiredTrialTenants(injectTx(context.Background(), tx), 15, 500)
	assert.Error(t, err)
}

// PruneOutbox was removed 2026-09-20 — outbox pruning goes entirely through
// platform-events' outbox.Runner.PrunePublished now (cmd/reconciler/jobs/
// outbox_prune.go); this hand-rolled DELETE had zero production callers.

// PruneProcessedEvents was removed — processed_events pruning goes through
// platform-events' inbox.Store.Prune (InboxRepository.Prune, covered in
// test/postgres TestInbox_ExactlyOnce).
