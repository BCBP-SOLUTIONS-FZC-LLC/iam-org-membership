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

func dmRow(tenantID, userID, deptID uuid.UUID, level string) []any {
	now := time.Now()
	return []any{
		uuid.New(), tenantID, userID, uuid.New(), deptID, level,
		uuid.New(), int64(1), now, now, (*time.Time)(nil),
	}
}

// ── ListByDepartment (P-9 backend) ─────────────────────────────────────

func TestDeptMembershipRepo_ListByDepartment_QueriesDepartmentFilter(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	var gotSQL string
	var gotArgs []any
	tx := &fakeTx{
		queryFn: func(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
			gotSQL = sql
			gotArgs = args
			return &fakeRows{scripted: [][]any{
				dmRow(tenantID, uuid.New(), deptID, "reviewer"),
				dmRow(tenantID, uuid.New(), deptID, "approver"),
			}}, nil
		},
	}
	repo := NewDeptMembershipRepository(nil)
	got, err := repo.ListByDepartment(injectTx(context.Background(), tx), tenantID, deptID)
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Contains(t, gotSQL, "department_id = $2")
	assert.Contains(t, gotSQL, "ORDER BY user_id")
	require.Len(t, gotArgs, 2)
	assert.Equal(t, tenantID, gotArgs[0])
	assert.Equal(t, deptID, gotArgs[1])
	assert.Equal(t, domain.DeptReviewer, got[0].RoleLevel)
	assert.Equal(t, domain.DeptApprover, got[1].RoleLevel)
}

// ── Remove (P-11 backend) ──────────────────────────────────────────────

func TestDeptMembershipRepo_Remove_SoftDeletesAndReturnsRow(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	var gotSQL string
	var gotArgs []any
	tx := &fakeTx{
		queryRowFn: func(_ context.Context, sql string, args ...any) pgx.Row {
			gotSQL = sql
			gotArgs = args
			return &fakeRow{values: dmRow(tenantID, userID, deptID, "preparator")}
		},
	}
	repo := NewDeptMembershipRepository(nil)
	got, err := repo.Remove(injectTx(context.Background(), tx), tenantID, userID, deptID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Contains(t, gotSQL, "UPDATE dept_memberships SET deleted_at")
	assert.Contains(t, gotSQL, "RETURNING")
	require.Len(t, gotArgs, 3)
	assert.Equal(t, deptID, got.DepartmentID)
}

func TestDeptMembershipRepo_Remove_NoRowsMapsToErrMemberNotFound(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := NewDeptMembershipRepository(nil).
		Remove(injectTx(context.Background(), tx), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrMemberNotFound)
}

func TestDeptMembershipRepo_Remove_UnrecognizedErrorPassesThrough(t *testing.T) {
	removeErr := errors.New("remove failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: removeErr}
		},
	}
	_, err := NewDeptMembershipRepository(nil).
		Remove(injectTx(context.Background(), tx), uuid.New(), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, removeErr)
}

// ── listWhere error paths (via ListByUser) ──────────────────────────────

func TestDeptMembershipRepo_ListByUser_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewDeptMembershipRepository(nil).
		ListByUser(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, queryErr)
}

func TestDeptMembershipRepo_ListByUser_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil // too few columns
		},
	}
	_, err := NewDeptMembershipRepository(nil).
		ListByUser(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.Error(t, err)
}

func TestDeptMembershipRepo_ListByUser_IterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("iteration failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewDeptMembershipRepository(nil).
		ListByUser(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, iterErr)
}

// ── Assign (B15 concurrency-safe upsert) ────────────────────────────────

func TestDeptMembershipRepo_Assign_FreshGrantNoExistingRow(t *testing.T) {
	tenantID, userID, deptID, memID, grantedBy := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	queryRowCalls := 0
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			queryRowCalls++
			switch queryRowCalls {
			case 1: // FOR UPDATE probe — no existing row
				return &fakeRow{err: pgx.ErrNoRows}
			default: // INSERT RETURNING
				return &fakeRow{values: dmRow(tenantID, userID, deptID, "preparator")}
			}
		},
	}
	out, previous, err := NewDeptMembershipRepository(nil).
		Assign(injectTx(context.Background(), tx), tenantID, userID, deptID, memID, domain.DeptPreparator, grantedBy)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Nil(t, previous, "fresh grant has no previous row")
}

func TestDeptMembershipRepo_Assign_SameLevelReturnsExistingWithoutMutation(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	execCalls := 0
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			execCalls++
			return pgconn.CommandTag{}, nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: dmRow(tenantID, userID, deptID, "reviewer")}
		},
	}
	out, previous, err := NewDeptMembershipRepository(nil).
		Assign(injectTx(context.Background(), tx), tenantID, userID, deptID, uuid.New(), domain.DeptReviewer, uuid.New())
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, previous)
	assert.Equal(t, domain.DeptReviewer, out.RoleLevel)
	assert.Equal(t, 1, execCalls, "only the advisory lock exec — no soft-delete for a same-level call")
}

func TestDeptMembershipRepo_Assign_LevelChangeSoftDeletesThenInserts(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	queryRowCalls := 0
	execCalls := 0
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			execCalls++
			return pgconn.CommandTag{}, nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			queryRowCalls++
			switch queryRowCalls {
			case 1: // FOR UPDATE probe — existing row at a different level
				return &fakeRow{values: dmRow(tenantID, userID, deptID, "preparator")}
			default: // INSERT RETURNING new row
				return &fakeRow{values: dmRow(tenantID, userID, deptID, "approver")}
			}
		},
	}
	out, previous, err := NewDeptMembershipRepository(nil).
		Assign(injectTx(context.Background(), tx), tenantID, userID, deptID, uuid.New(), domain.DeptApprover, uuid.New())
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, previous)
	assert.Equal(t, domain.DeptPreparator, previous.RoleLevel)
	assert.Equal(t, domain.DeptApprover, out.RoleLevel)
	assert.Equal(t, 2, execCalls, "advisory lock + soft-delete of the old row")
}

func TestDeptMembershipRepo_Assign_AdvisoryLockErrorPassesThrough(t *testing.T) {
	lockErr := errors.New("advisory lock failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, lockErr
		},
	}
	_, _, err := NewDeptMembershipRepository(nil).
		Assign(injectTx(context.Background(), tx), uuid.New(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())
	assert.ErrorIs(t, err, lockErr)
}

func TestDeptMembershipRepo_Assign_ForUpdateUnrecognizedErrorPassesThrough(t *testing.T) {
	probeErr := errors.New("lock probe failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: probeErr}
		},
	}
	_, _, err := NewDeptMembershipRepository(nil).
		Assign(injectTx(context.Background(), tx), uuid.New(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())
	assert.ErrorIs(t, err, probeErr)
}

func TestDeptMembershipRepo_Assign_SoftDeleteExecErrorPassesThrough(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	deleteErr := errors.New("soft delete failed")
	execCalls := 0
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			execCalls++
			if execCalls == 1 {
				return pgconn.CommandTag{}, nil // advisory lock
			}
			return pgconn.CommandTag{}, deleteErr // soft delete
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: dmRow(tenantID, userID, deptID, "preparator")}
		},
	}
	_, _, err := NewDeptMembershipRepository(nil).
		Assign(injectTx(context.Background(), tx), tenantID, userID, deptID, uuid.New(), domain.DeptApprover, uuid.New())
	assert.ErrorIs(t, err, deleteErr)
}

func TestDeptMembershipRepo_Assign_InsertUnrecognizedErrorPassesThrough(t *testing.T) {
	insertErr := errors.New("insert failed")
	queryRowCalls := 0
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			queryRowCalls++
			if queryRowCalls == 1 {
				return &fakeRow{err: pgx.ErrNoRows} // no existing row
			}
			return &fakeRow{err: insertErr} // insert fails
		},
	}
	_, _, err := NewDeptMembershipRepository(nil).
		Assign(injectTx(context.Background(), tx), uuid.New(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())
	assert.ErrorIs(t, err, insertErr)
}

func TestDeptMembershipRepo_Assign_InsertAbsorbedByConflictFetchesWinner(t *testing.T) {
	tenantID, userID, deptID := uuid.New(), uuid.New(), uuid.New()
	queryRowCalls := 0
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			queryRowCalls++
			switch queryRowCalls {
			case 1:
				return &fakeRow{err: pgx.ErrNoRows} // no existing row
			case 2:
				return &fakeRow{err: pgx.ErrNoRows} // insert absorbed by ON CONFLICT
			default:
				return &fakeRow{values: dmRow(tenantID, userID, deptID, "preparator")} // winner fetch
			}
		},
	}
	out, previous, err := NewDeptMembershipRepository(nil).
		Assign(injectTx(context.Background(), tx), tenantID, userID, deptID, uuid.New(), domain.DeptPreparator, uuid.New())
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Nil(t, previous)
}

func TestDeptMembershipRepo_Assign_WinnerFetchErrorPassesThrough(t *testing.T) {
	winnerErr := errors.New("winner fetch failed")
	queryRowCalls := 0
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			queryRowCalls++
			switch queryRowCalls {
			case 1:
				return &fakeRow{err: pgx.ErrNoRows}
			case 2:
				return &fakeRow{err: pgx.ErrNoRows}
			default:
				return &fakeRow{err: winnerErr}
			}
		},
	}
	_, _, err := NewDeptMembershipRepository(nil).
		Assign(injectTx(context.Background(), tx), uuid.New(), uuid.New(), uuid.New(), uuid.New(), domain.DeptPreparator, uuid.New())
	assert.ErrorIs(t, err, winnerErr)
}

// ── SoftDeleteAllForDept (GAP-P25-1 dept deactivation cascade) ──────────

func TestDeptMembershipRepo_SoftDeleteAllForDept_ReturnsSoftDeletedRows(t *testing.T) {
	tenantID, deptID := uuid.New(), uuid.New()
	var gotSQL string
	var gotArgs []any
	tx := &fakeTx{
		queryFn: func(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
			gotSQL = sql
			gotArgs = args
			return &fakeRows{scripted: [][]any{
				dmRow(tenantID, uuid.New(), deptID, "preparator"),
				dmRow(tenantID, uuid.New(), deptID, "reviewer"),
			}}, nil
		},
	}
	got, err := NewDeptMembershipRepository(nil).
		SoftDeleteAllForDept(injectTx(context.Background(), tx), tenantID, deptID)
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Contains(t, gotSQL, "UPDATE dept_memberships SET deleted_at")
	assert.Contains(t, gotSQL, "department_id = $2")
	require.Len(t, gotArgs, 2)
	assert.Equal(t, tenantID, gotArgs[0])
	assert.Equal(t, deptID, gotArgs[1])
}

func TestDeptMembershipRepo_SoftDeleteAllForDept_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewDeptMembershipRepository(nil).
		SoftDeleteAllForDept(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, queryErr)
}

func TestDeptMembershipRepo_SoftDeleteAllForDept_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil
		},
	}
	_, err := NewDeptMembershipRepository(nil).
		SoftDeleteAllForDept(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.Error(t, err)
}

func TestDeptMembershipRepo_SoftDeleteAllForDept_IterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("iteration failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewDeptMembershipRepository(nil).
		SoftDeleteAllForDept(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, iterErr)
}

// ── SoftDeleteAllForUser error paths (cascade-only, I-5/P-7) ────────────

func TestDeptMembershipRepo_SoftDeleteAllForUser_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewDeptMembershipRepository(nil).
		SoftDeleteAllForUser(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, queryErr)
}

func TestDeptMembershipRepo_SoftDeleteAllForUser_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil
		},
	}
	_, err := NewDeptMembershipRepository(nil).
		SoftDeleteAllForUser(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.Error(t, err)
}

func TestDeptMembershipRepo_SoftDeleteAllForUser_IterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("iteration failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewDeptMembershipRepository(nil).
		SoftDeleteAllForUser(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, iterErr)
}

func TestDeptMembershipRepo_SoftDeleteAllForUser_ReturnsSoftDeletedRows(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{
				dmRow(tenantID, userID, uuid.New(), "preparator"),
			}}, nil
		},
	}
	got, err := NewDeptMembershipRepository(nil).
		SoftDeleteAllForUser(injectTx(context.Background(), tx), tenantID, userID)
	require.NoError(t, err)
	assert.Len(t, got, 1)
}
