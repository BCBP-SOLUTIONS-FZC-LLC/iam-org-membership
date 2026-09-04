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

// ── ListByRole — TM-8 owner-count lookup ────────────────────────────────

func TestTenantRoleRepo_ListByRole_ExecutesRoleFilteredQuery(t *testing.T) {
	tenantID, memID, ownerID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now()

	var gotSQL string
	var gotArgs []any
	tx := &fakeTx{
		queryFn: func(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
			gotSQL = sql
			gotArgs = args
			// Return one owner row matching the SELECT column order.
			return &fakeRows{
				scripted: [][]any{
					{
						uuid.New(), tenantID, ownerID, memID, "tenant_owner",
						ownerID, int64(1), now, now, (*time.Time)(nil),
					},
				},
			}, nil
		},
	}
	repo := NewTenantRoleRepository(nil)
	ctx := injectTx(context.Background(), tx)

	got, err := repo.ListByRole(ctx, tenantID, domain.RoleTenantOwner)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, gotSQL, "role_code = $2")
	assert.Contains(t, gotSQL, "deleted_at IS NULL")
	require.Len(t, gotArgs, 2)
	assert.Equal(t, tenantID, gotArgs[0])
	assert.Equal(t, "tenant_owner", gotArgs[1],
		"ListByRole must pass the string form of the enum, not the typed value")
	assert.Equal(t, domain.RoleTenantOwner, got[0].RoleCode)
}

// CRITICAL: an unrecognized error from the underlying Query call — the same
// shape a caller's own RunInTx/withPool callback could return for its own
// business reasons — must pass through wrapConnErr unchanged, not get
// silently reclassified as ErrDependencyUnavailable (see db_test.go's
// TestWrapConnErr_UnrecognizedGenericErrorPassesThroughUnchanged for the
// full rationale — this test pins the same contract at the repository
// layer specifically).
func TestTenantRoleRepo_ListByRole_UnrecognizedQueryErrorPassesThroughUnchanged(t *testing.T) {
	queryErr := errors.New("dial tcp: connection refused")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	repo := NewTenantRoleRepository(nil)
	_, err := repo.ListByRole(injectTx(context.Background(), tx), uuid.New(), domain.RoleTenantAdmin)
	assert.ErrorIs(t, err, queryErr)
}

func TestTenantRoleRepo_ListByRole_PgxErrNoRowsPassesThrough(t *testing.T) {
	// pgx.ErrNoRows must NOT get remapped to ErrDependencyUnavailable —
	// the service layer distinguishes "row missing" from "db down".
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, pgx.ErrNoRows
		},
	}
	repo := NewTenantRoleRepository(nil)
	_, err := repo.ListByRole(injectTx(context.Background(), tx), uuid.New(), domain.RoleTenantAdmin)
	assert.ErrorIs(t, err, pgx.ErrNoRows)
}

func TestTenantRoleRepo_ListByRole_PropagatesScanError(t *testing.T) {
	// scripted values shorter than the SELECT column count → Scan error.
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil
		},
	}
	repo := NewTenantRoleRepository(nil)
	_, err := repo.ListByRole(injectTx(context.Background(), tx), uuid.New(), domain.RoleTenantAdmin)
	assert.Error(t, err)
}

func TestTenantRoleRepo_ListByRole_RowsIteratorErrorPassesThroughUnchanged(t *testing.T) {
	// A rows.Err() mid-stream is, like the Query-level case above, not
	// positively identifiable as a connectivity failure — it flows through
	// wrapConnErr unchanged rather than getting silently reclassified.
	iterErr := errors.New("network reset mid-stream")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	repo := NewTenantRoleRepository(nil)
	_, err := repo.ListByRole(injectTx(context.Background(), tx), uuid.New(), domain.RoleTenantAdmin)
	assert.ErrorIs(t, err, iterErr)
}

// tenantRoleRow builds a scripted row matching scanTenantRole's Scan order
// (10 fields).
func tenantRoleRow(tenantID, userID uuid.UUID, role string, rv int64) []any {
	now := time.Now()
	return []any{
		uuid.New(), tenantID, userID, uuid.New(), role,
		uuid.New(), rv, now, now, (*time.Time)(nil),
	}
}

// ── Grant (idempotent, ON CONFLICT DO NOTHING) ──────────────────────────

func TestTenantRoleRepo_Grant_ReturnsCreatedRowOnFreshInsert(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: tenantRoleRow(tenantID, userID, "tenant_admin", 1)}
		},
	}
	got, err := NewTenantRoleRepository(nil).Grant(injectTx(context.Background(), tx), &domain.TenantRole{
		TenantID: tenantID, UserID: userID, RoleCode: domain.RoleTenantAdmin,
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.RoleTenantAdmin, got.RoleCode)
}

func TestTenantRoleRepo_Grant_UnrecognizedScanErrorPassesThrough(t *testing.T) {
	insertErr := errors.New("insert failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: insertErr}
		},
	}
	_, err := NewTenantRoleRepository(nil).Grant(injectTx(context.Background(), tx), &domain.TenantRole{
		TenantID: uuid.New(), UserID: uuid.New(), RoleCode: domain.RoleTenantAdmin,
	})
	assert.ErrorIs(t, err, insertErr)
}

func TestTenantRoleRepo_Grant_ConflictAbsorbedFetchesExistingRow(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{err: pgx.ErrNoRows}
			}
			return &fakeRow{values: tenantRoleRow(tenantID, userID, "tenant_admin", 2)}
		},
	}
	got, err := NewTenantRoleRepository(nil).Grant(injectTx(context.Background(), tx), &domain.TenantRole{
		TenantID: tenantID, UserID: userID, RoleCode: domain.RoleTenantAdmin,
	})
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestTenantRoleRepo_Grant_ConflictAbsorbedWinnerFetchErrorPassesThrough(t *testing.T) {
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
	_, err := NewTenantRoleRepository(nil).Grant(injectTx(context.Background(), tx), &domain.TenantRole{
		TenantID: uuid.New(), UserID: uuid.New(), RoleCode: domain.RoleTenantAdmin,
	})
	assert.ErrorIs(t, err, winnerErr)
}

// ── Revoke ────────────────────────────────────────────────────────────

func TestTenantRoleRepo_Revoke_SoftDeletesAndReturnsRow(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: tenantRoleRow(tenantID, userID, "tender_admin", 1)}
		},
	}
	got, err := NewTenantRoleRepository(nil).
		Revoke(injectTx(context.Background(), tx), tenantID, userID, domain.RoleTenderAdmin)
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestTenantRoleRepo_Revoke_NoRowsMapsToMemberNotFound(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := NewTenantRoleRepository(nil).
		Revoke(injectTx(context.Background(), tx), uuid.New(), uuid.New(), domain.RoleTenderAdmin)
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

func TestTenantRoleRepo_Revoke_UnrecognizedErrorPassesThrough(t *testing.T) {
	revokeErr := errors.New("revoke failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: revokeErr}
		},
	}
	_, err := NewTenantRoleRepository(nil).
		Revoke(injectTx(context.Background(), tx), uuid.New(), uuid.New(), domain.RoleTenderAdmin)
	assert.ErrorIs(t, err, revokeErr)
}

// ── SoftDeleteAllForUser (I-5/P-7 cascade-only) ─────────────────────────

func TestTenantRoleRepo_SoftDeleteAllForUser_ReturnsSoftDeletedRows(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{
				tenantRoleRow(tenantID, userID, "tenant_admin", 1),
			}}, nil
		},
	}
	got, err := NewTenantRoleRepository(nil).
		SoftDeleteAllForUser(injectTx(context.Background(), tx), tenantID, userID)
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestTenantRoleRepo_SoftDeleteAllForUser_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewTenantRoleRepository(nil).
		SoftDeleteAllForUser(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, queryErr)
}

func TestTenantRoleRepo_SoftDeleteAllForUser_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil
		},
	}
	_, err := NewTenantRoleRepository(nil).
		SoftDeleteAllForUser(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.Error(t, err)
}

func TestTenantRoleRepo_SoftDeleteAllForUser_IterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("iteration failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewTenantRoleRepository(nil).
		SoftDeleteAllForUser(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, iterErr)
}
