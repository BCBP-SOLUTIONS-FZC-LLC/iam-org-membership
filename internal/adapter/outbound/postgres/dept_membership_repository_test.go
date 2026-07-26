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
