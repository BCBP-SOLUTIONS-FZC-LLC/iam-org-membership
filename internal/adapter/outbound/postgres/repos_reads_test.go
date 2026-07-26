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

// delegationRow builds a scripted row matching the SELECT column order of
// delegationCols (16 fields).
func delegationRow(tenantID, delegatorID, delegateID uuid.UUID, scope, status string) []any {
	now := time.Now()
	var reason *string
	return []any{
		uuid.New(), tenantID, delegatorID, delegateID,
		uuid.New(), uuid.New(),
		scope, (*uuid.UUID)(nil), reason, now, (*time.Time)(nil), status,
		int64(1), now, now, (*time.Time)(nil),
	}
}

// ── DelegationRepository.ListByDelegator ────────────────────────────────

func TestDelegationRepo_ListByDelegator_QueriesFilteredByDelegator(t *testing.T) {
	tenantID, delegatorID := uuid.New(), uuid.New()
	var gotSQL string
	var gotArgs []any
	tx := &fakeTx{
		queryFn: func(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
			gotSQL = sql
			gotArgs = args
			return &fakeRows{scripted: [][]any{
				delegationRow(tenantID, delegatorID, uuid.New(), "all", "active"),
			}}, nil
		},
	}
	repo := NewDelegationRepository(nil)
	got, err := repo.ListByDelegator(injectTx(context.Background(), tx), tenantID, delegatorID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, gotSQL, "delegator_id = $2")
	assert.Contains(t, gotSQL, "ORDER BY starts_at DESC")
	require.Len(t, gotArgs, 2)
	assert.Equal(t, tenantID, gotArgs[0])
	assert.Equal(t, delegatorID, gotArgs[1])
}

func TestDelegationRepo_ListByDelegator_EmptyResultReturnsNilSlice(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: nil}, nil
		},
	}
	got, err := NewDelegationRepository(nil).
		ListByDelegator(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Empty(t, got)
}

// ── DelegationRepository.FindActiveDeptDelegateForUser (WFI-11) ─────────

func TestDelegationRepo_FindActiveDeptDelegateForUser_ScopeAndScopeIDArg(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	var gotSQL string
	var gotArgs []any
	tx := &fakeTx{
		queryRowFn: func(_ context.Context, sql string, args ...any) pgx.Row {
			gotSQL = sql
			gotArgs = args
			return &fakeRow{values: delegationRow(tenantID, uuid.New(), userID, "department", "active")}
		},
	}
	repo := NewDelegationRepository(nil)
	got, err := repo.FindActiveDeptDelegateForUser(injectTx(context.Background(), tx), tenantID, userID, deptID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Contains(t, gotSQL, "scope = 'department'")
	assert.Contains(t, gotSQL, "status = 'active'")
	assert.Contains(t, gotSQL, "LIMIT 1")
	require.Len(t, gotArgs, 3)
	assert.Equal(t, tenantID, gotArgs[0])
	assert.Equal(t, userID, gotArgs[1])
	assert.Equal(t, deptID, gotArgs[2])
}

func TestDelegationRepo_FindActiveDeptDelegateForUser_NoRowsReturnsNilNoError(t *testing.T) {
	// Contract per source: (nil, nil) when no active dept delegation exists.
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	repo := NewDelegationRepository(nil)
	got, err := repo.FindActiveDeptDelegateForUser(injectTx(context.Background(), tx), uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Nil(t, got, "no active delegation must return (nil, nil), not ErrNoRows")
}

func TestDelegationRepo_FindActiveDeptDelegateForUser_NonNoRowsError(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: errors.New("boom")}
		},
	}
	_, err := NewDelegationRepository(nil).
		FindActiveDeptDelegateForUser(injectTx(context.Background(), tx), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrDependencyUnavailable)
}
