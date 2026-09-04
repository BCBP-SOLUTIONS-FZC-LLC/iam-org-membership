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

func TestMembershipRepo_probeMembership_UnrecognizedErrorPassesThrough(t *testing.T) {
	probeErr := errors.New("probe failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: probeErr}
		},
	}
	err := (&MembershipRepository{}).probeMembership(context.Background(), tx, uuid.New(), uuid.New())
	assert.ErrorIs(t, err, probeErr)
}

// ── List error paths ──────────────────────────────────────────────────

func TestMembershipRepo_List_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewMembershipRepository(nil).List(injectTx(context.Background(), tx), uuid.New(), nil, 50)
	assert.ErrorIs(t, err, queryErr)
}

func TestMembershipRepo_List_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil
		},
	}
	_, err := NewMembershipRepository(nil).List(injectTx(context.Background(), tx), uuid.New(), nil, 50)
	assert.Error(t, err)
}

func TestMembershipRepo_List_IterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("iteration failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewMembershipRepository(nil).List(injectTx(context.Background(), tx), uuid.New(), nil, 50)
	assert.ErrorIs(t, err, iterErr)
}

// ── FindByUserID ──────────────────────────────────────────────────────

func TestMembershipRepo_FindByUserID_ReturnsRow(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: membershipRow(tenantID, userID, "active")}
		},
	}
	got, err := NewMembershipRepository(nil).FindByUserID(injectTx(context.Background(), tx), tenantID, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.MembershipStatus("active"), got.Status)
}

func TestMembershipRepo_FindByUserID_NoRowsMapsToMemberNotFound(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := NewMembershipRepository(nil).FindByUserID(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

func TestMembershipRepo_FindByUserID_UnrecognizedErrorPassesThrough(t *testing.T) {
	findErr := errors.New("find failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: findErr}
		},
	}
	_, err := NewMembershipRepository(nil).FindByUserID(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, findErr)
}

// ── Insert (PI-10 idempotent) ─────────────────────────────────────────

func TestMembershipRepo_Insert_ReturnsCreatedRowOnFreshInsert(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: membershipRow(tenantID, userID, "active")}
		},
	}
	got, err := NewMembershipRepository(nil).Insert(injectTx(context.Background(), tx), &domain.TenantMembership{
		TenantID: tenantID, UserID: userID, Status: domain.MembershipStatus("active"),
	})
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestMembershipRepo_Insert_UnrecognizedScanErrorPassesThrough(t *testing.T) {
	insertErr := errors.New("insert failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: insertErr}
		},
	}
	_, err := NewMembershipRepository(nil).Insert(injectTx(context.Background(), tx), &domain.TenantMembership{
		TenantID: uuid.New(), UserID: uuid.New(),
	})
	assert.ErrorIs(t, err, insertErr)
}

func TestMembershipRepo_Insert_ConflictAbsorbedFetchesExistingRow(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{err: pgx.ErrNoRows} // ON CONFLICT DO NOTHING absorbed
			}
			return &fakeRow{values: membershipRow(tenantID, userID, "suspended")}
		},
	}
	got, err := NewMembershipRepository(nil).Insert(injectTx(context.Background(), tx), &domain.TenantMembership{
		TenantID: tenantID, UserID: userID, Status: domain.MembershipStatus("active"),
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.MembershipStatus("suspended"), got.Status, "existing row's status must not be laundered")
}

func TestMembershipRepo_Insert_ConflictAbsorbedWinnerFetchErrorPassesThrough(t *testing.T) {
	winnerErr := errors.New("winner fetch failed")
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{err: pgx.ErrNoRows}
			}
			return &fakeRow{err: winnerErr}
		},
	}
	_, err := NewMembershipRepository(nil).Insert(injectTx(context.Background(), tx), &domain.TenantMembership{
		TenantID: uuid.New(), UserID: uuid.New(),
	})
	assert.ErrorIs(t, err, winnerErr)
}

// ── SetStatus ──────────────────────────────────────────────────────────

func TestMembershipRepo_SetStatus_SuccessOnMatch(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: membershipRow(tenantID, userID, "suspended")}
		},
	}
	got, err := NewMembershipRepository(nil).
		SetStatus(injectTx(context.Background(), tx), tenantID, userID, domain.MembershipStatus("suspended"), 1)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.MembershipStatus("suspended"), got.Status)
}

func TestMembershipRepo_SetStatus_NoRowsFallsThroughToProbe(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := NewMembershipRepository(nil).
		SetStatus(injectTx(context.Background(), tx), uuid.New(), uuid.New(), domain.MembershipStatus("suspended"), 1)
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

func TestMembershipRepo_SetStatus_UnrecognizedScanErrorPassesThrough(t *testing.T) {
	scanErr := errors.New("scan failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: scanErr}
		},
	}
	_, err := NewMembershipRepository(nil).
		SetStatus(injectTx(context.Background(), tx), uuid.New(), uuid.New(), domain.MembershipStatus("suspended"), 1)
	assert.ErrorIs(t, err, scanErr)
}

// ── SoftDelete ─────────────────────────────────────────────────────────

func TestMembershipRepo_SoftDelete_SuccessWhenRowAffected(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	err := NewMembershipRepository(nil).
		SoftDelete(injectTx(context.Background(), tx), uuid.New(), uuid.New(), 1)
	assert.NoError(t, err)
}

func TestMembershipRepo_SoftDelete_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	err := NewMembershipRepository(nil).
		SoftDelete(injectTx(context.Background(), tx), uuid.New(), uuid.New(), 1)
	assert.ErrorIs(t, err, execErr)
}

func TestMembershipRepo_SoftDelete_NoRowsFallsThroughToProbe(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	err := NewMembershipRepository(nil).
		SoftDelete(injectTx(context.Background(), tx), uuid.New(), uuid.New(), 1)
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}
