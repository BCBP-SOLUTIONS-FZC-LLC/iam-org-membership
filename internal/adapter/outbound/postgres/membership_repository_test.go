package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// membershipRow matches the SELECT column order in membershipCols (8 fields).
func membershipRow(tenantID, userID uuid.UUID, status string) []any {
	now := time.Now()
	return []any{
		uuid.New(), tenantID, userID, status,
		int64(1), now, now, (*time.Time)(nil),
	}
}

// ── List (P-4) — keyset-paginated ──────────────────────────────────────

func TestMembershipRepo_List_FirstPageQueryHasNoCursor(t *testing.T) {
	tenantID := uuid.New()
	var gotSQL string
	var gotArgs []any
	tx := &fakeTx{
		queryFn: func(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
			gotSQL = sql
			gotArgs = args
			return &fakeRows{scripted: [][]any{
				membershipRow(tenantID, uuid.New(), "active"),
			}}, nil
		},
	}
	page, err := NewMembershipRepository(nil).
		List(injectTx(context.Background(), tx), tenantID, nil, 50)
	require.NoError(t, err)
	require.NotNil(t, page)
	assert.Len(t, page.Items, 1)
	assert.Nil(t, page.NextCursor, "single row fits on one page")
	assert.Contains(t, gotSQL, "ORDER BY created_at ASC, id ASC")
	assert.NotContains(t, gotSQL, "(created_at, id) >", "no-cursor path has no keyset predicate")
	require.Len(t, gotArgs, 2)
	assert.Equal(t, tenantID, gotArgs[0])
	assert.Equal(t, 51, gotArgs[1], "requests limit+1 to detect a next page")
}

func TestMembershipRepo_List_CursorPageUsesKeysetPredicate(t *testing.T) {
	tenantID := uuid.New()
	cursor := &domain.MembershipListCursor{CreatedAt: time.Now(), ID: uuid.New()}
	var gotSQL string
	var gotArgs []any
	tx := &fakeTx{
		queryFn: func(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
			gotSQL = sql
			gotArgs = args
			return &fakeRows{}, nil
		},
	}
	_, err := NewMembershipRepository(nil).
		List(injectTx(context.Background(), tx), tenantID, cursor, 25)
	require.NoError(t, err)
	assert.Contains(t, gotSQL, "(created_at, id) > ($2, $3)")
	require.Len(t, gotArgs, 4)
	assert.Equal(t, tenantID, gotArgs[0])
	assert.Equal(t, cursor.CreatedAt, gotArgs[1])
	assert.Equal(t, cursor.ID, gotArgs[2])
	assert.Equal(t, 26, gotArgs[3])
}

func TestMembershipRepo_List_InvalidLimitDefaultsTo50(t *testing.T) {
	var gotArgs []any
	tx := &fakeTx{
		queryFn: func(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
			gotArgs = args
			return &fakeRows{}, nil
		},
	}
	// limit=0 (invalid) → default 50 → query passes limit+1=51.
	_, err := NewMembershipRepository(nil).
		List(injectTx(context.Background(), tx), uuid.New(), nil, 0)
	require.NoError(t, err)
	assert.Equal(t, 51, gotArgs[1])
}

func TestMembershipRepo_List_LimitTooLargeDefaultsTo50(t *testing.T) {
	var gotArgs []any
	tx := &fakeTx{
		queryFn: func(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
			gotArgs = args
			return &fakeRows{}, nil
		},
	}
	// limit=300 > 200 cap → default 50.
	_, err := NewMembershipRepository(nil).
		List(injectTx(context.Background(), tx), uuid.New(), nil, 300)
	require.NoError(t, err)
	assert.Equal(t, 51, gotArgs[1])
}

func TestMembershipRepo_List_TrimsExtraRowAndSetsNextCursor(t *testing.T) {
	tenantID := uuid.New()
	// Returning limit+1 rows must trim the last and produce a NextCursor
	// pointing at the last KEPT row.
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			rows := [][]any{}
			for range 3 {
				rows = append(rows, membershipRow(tenantID, uuid.New(), "active"))
			}
			return &fakeRows{scripted: rows}, nil
		},
	}
	page, err := NewMembershipRepository(nil).
		List(injectTx(context.Background(), tx), tenantID, nil, 2)
	require.NoError(t, err)
	require.Len(t, page.Items, 2, "extra row must be trimmed to leave exactly limit items")
	require.NotNil(t, page.NextCursor, "NextCursor set when the extra row was returned")
}

// ── probeMembership — unexported PI-8-style helper (whitebox) ──────────

func TestMembershipRepo_probeMembership_MissingRowMapsToMemberNotFound(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	err := (&MembershipRepository{}).probeMembership(context.Background(), tx, uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

func TestMembershipRepo_probeMembership_ExistingRowMapsToOptimisticLockConflict(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: []any{int64(5)}}
		},
	}
	err := (&MembershipRepository{}).probeMembership(context.Background(), tx, uuid.New(), uuid.New())

	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
	assert.EqualValues(t, 5, de.Details["record_version"])
}
