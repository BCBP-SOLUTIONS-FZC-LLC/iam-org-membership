package postgres

import (
	"context"
	"encoding/json"
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

// inviteRow builds a scripted row matching the SELECT column order of
// inviteCols (15 fields). Non-nullable rolesArr must be non-nil so
// scanInvitation's copy loop is exercised.
func inviteRow(tenantID uuid.UUID, email string, status string, rv int64) []any {
	now := time.Now()
	expires := now.Add(7 * 24 * time.Hour)
	deptMappingsJSON, _ := json.Marshal([]domain.InvitationDeptMapping{})
	return []any{
		uuid.New(),         // id
		tenantID,           // tenant_id
		email,              // email
		"Test Person",      // full_name
		[]string{"member"}, // initial_tenant_roles
		deptMappingsJSON,   // initial_dept_mappings
		uuid.New(),         // invited_by
		(*uuid.UUID)(nil),  // keycloak_user_id
		status,             // status
		expires,            // expires_at
		(*time.Time)(nil),  // accepted_at
		false,              // kc_cleanup_pending
		rv,                 // record_version
		now,                // created_at
		now,                // updated_at
	}
}

// ── List (P-30) ─────────────────────────────────────────────────────────

func TestInvitationRepo_List_QueriesPendingInvitations(t *testing.T) {
	tenantID := uuid.New()
	var gotSQL string
	tx := &fakeTx{
		queryFn: func(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
			gotSQL = sql
			return &fakeRows{scripted: [][]any{
				inviteRow(tenantID, "a@x.com", "pending", 1),
				inviteRow(tenantID, "b@x.com", "pending", 2),
			}}, nil
		},
	}
	got, err := NewInvitationRepository(nil).List(injectTx(context.Background(), tx), tenantID)
	require.NoError(t, err)
	assert.Len(t, got, 2)
	assert.Contains(t, gotSQL, "status = 'pending'")
	assert.Contains(t, gotSQL, "ORDER BY created_at DESC")
	assert.Equal(t, "a@x.com", got[0].Email)
	assert.Equal(t, domain.InvitationStatus("pending"), got[0].Status)
}

// ── FindByID ───────────────────────────────────────────────────────────

func TestInvitationRepo_FindByID_ReturnsRow(t *testing.T) {
	tenantID, id := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: inviteRow(tenantID, "x@y.com", "pending", 3)}
		},
	}
	got, err := NewInvitationRepository(nil).FindByID(injectTx(context.Background(), tx), tenantID, id)
	require.NoError(t, err)
	assert.Equal(t, "x@y.com", got.Email)
}

func TestInvitationRepo_FindByID_NoRowsMapsToErrInvitationNotFound(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := NewInvitationRepository(nil).FindByID(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrInvitationNotFound)
}

// ── SetKeycloakUserID — optimistic-locked UPDATE with PI-8 probe ───────

func TestInvitationRepo_SetKeycloakUserID_SuccessOnMatch(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	err := NewInvitationRepository(nil).
		SetKeycloakUserID(injectTx(context.Background(), tx), uuid.New(), uuid.New(), uuid.New(), 1)
	assert.NoError(t, err)
}

func TestInvitationRepo_SetKeycloakUserID_NoRowsProbesForConflictOrMissing(t *testing.T) {
	// UPDATE affected 0 rows → probe queries record_version. If the row
	// exists at a different version, expect ErrOptimisticLockConflict.
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: []any{int64(7)}} // current record_version
		},
	}
	err := NewInvitationRepository(nil).
		SetKeycloakUserID(injectTx(context.Background(), tx), uuid.New(), uuid.New(), uuid.New(), 1)
	assert.ErrorIs(t, err, domain.ErrOptimisticLockConflict)
}

func TestInvitationRepo_SetKeycloakUserID_NoRowsProbeMissingMapsToNotFound(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	err := NewInvitationRepository(nil).
		SetKeycloakUserID(injectTx(context.Background(), tx), uuid.New(), uuid.New(), uuid.New(), 1)
	assert.ErrorIs(t, err, domain.ErrInvitationNotFound)
}

// ── SetKCCleanupPending — same PI-8 shape as SetKeycloakUserID ─────────

func TestInvitationRepo_SetKCCleanupPending_SuccessOnMatch(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	err := NewInvitationRepository(nil).
		SetKCCleanupPending(injectTx(context.Background(), tx), uuid.New(), uuid.New(), true, 1)
	assert.NoError(t, err)
}

func TestInvitationRepo_SetKCCleanupPending_ConflictWhenProbeReturnsDifferentVersion(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: []any{int64(9)}}
		},
	}
	err := NewInvitationRepository(nil).
		SetKCCleanupPending(injectTx(context.Background(), tx), uuid.New(), uuid.New(), false, 1)
	assert.ErrorIs(t, err, domain.ErrOptimisticLockConflict)
}

func TestInvitationRepo_SetKCCleanupPending_MissingRowMapsToNotFound(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	err := NewInvitationRepository(nil).
		SetKCCleanupPending(injectTx(context.Background(), tx), uuid.New(), uuid.New(), true, 1)
	assert.ErrorIs(t, err, domain.ErrInvitationNotFound)
}

// ── ListExpiring — invitation-expiry cron query ─────────────────────────

func TestInvitationRepo_ListExpiring_UsesBeforeCutoffAndLimit(t *testing.T) {
	cutoff := time.Now()
	var gotSQL string
	var gotArgs []any
	tx := &fakeTx{
		queryFn: func(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
			gotSQL = sql
			gotArgs = args
			return &fakeRows{scripted: [][]any{
				inviteRow(uuid.New(), "expired@x.com", "pending", 1),
			}}, nil
		},
	}
	got, err := NewInvitationRepository(nil).
		ListExpiring(injectTx(context.Background(), tx), cutoff, 100)
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Contains(t, gotSQL, "expires_at < $1")
	assert.Contains(t, gotSQL, "LIMIT 100")
	require.Len(t, gotArgs, 1)
	assert.WithinDuration(t, cutoff, gotArgs[0].(time.Time), time.Second)
}

// ── ListPendingKCCleanup — invitation-kc-cleanup reconciler query ──────

func TestInvitationRepo_ListPendingKCCleanup_FiltersByFlag(t *testing.T) {
	var gotSQL string
	tx := &fakeTx{
		queryFn: func(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
			gotSQL = sql
			return &fakeRows{scripted: [][]any{
				inviteRow(uuid.New(), "pending@x.com", "revoked", 3),
			}}, nil
		},
	}
	got, err := NewInvitationRepository(nil).
		ListPendingKCCleanup(injectTx(context.Background(), tx), 50)
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Contains(t, gotSQL, "kc_cleanup_pending = true")
	assert.Contains(t, gotSQL, "LIMIT 50")
}

func TestInvitationRepo_ListPendingKCCleanup_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewInvitationRepository(nil).ListPendingKCCleanup(injectTx(context.Background(), tx), 50)
	assert.ErrorIs(t, err, queryErr)
}

func TestInvitationRepo_ListPendingKCCleanup_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil
		},
	}
	_, err := NewInvitationRepository(nil).ListPendingKCCleanup(injectTx(context.Background(), tx), 50)
	assert.Error(t, err)
}

func TestInvitationRepo_ListPendingKCCleanup_IterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("iteration failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewInvitationRepository(nil).ListPendingKCCleanup(injectTx(context.Background(), tx), 50)
	assert.ErrorIs(t, err, iterErr)
}

// ── List error paths ─────────────────────────────────────────────────────

func TestInvitationRepo_List_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewInvitationRepository(nil).List(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, queryErr)
}

func TestInvitationRepo_List_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil
		},
	}
	_, err := NewInvitationRepository(nil).List(injectTx(context.Background(), tx), uuid.New())
	assert.Error(t, err)
}

func TestInvitationRepo_List_IterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("iteration failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewInvitationRepository(nil).List(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, iterErr)
}

// ── LockByID ───────────────────────────────────────────────────────────

func TestInvitationRepo_LockByID_ReturnsRowWhenFound(t *testing.T) {
	tenantID, id := uuid.New(), uuid.New()
	var gotSQL string
	tx := &fakeTx{
		queryRowFn: func(_ context.Context, sql string, _ ...any) pgx.Row {
			gotSQL = sql
			return &fakeRow{values: inviteRow(tenantID, "lock@x.com", "pending", 1)}
		},
	}
	got, err := NewInvitationRepository(nil).LockByID(injectTx(context.Background(), tx), id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Contains(t, gotSQL, "FOR UPDATE")
	assert.Equal(t, "lock@x.com", got.Email)
}

func TestInvitationRepo_LockByID_NoRowsReturnsNilNil(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	got, err := NewInvitationRepository(nil).LockByID(injectTx(context.Background(), tx), uuid.New())
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestInvitationRepo_LockByID_UnrecognizedErrorPassesThrough(t *testing.T) {
	lockErr := errors.New("lock failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: lockErr}
		},
	}
	_, err := NewInvitationRepository(nil).LockByID(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, lockErr)
}

// ── FindByID error path ──────────────────────────────────────────────────

func TestInvitationRepo_FindByID_UnrecognizedErrorPassesThrough(t *testing.T) {
	findErr := errors.New("find failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: findErr}
		},
	}
	_, err := NewInvitationRepository(nil).FindByID(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, findErr)
}

// ── FindPendingByEmail (PI-4 acceptance discriminator) ──────────────────

func TestInvitationRepo_FindPendingByEmail_ReturnsRowWhenFound(t *testing.T) {
	tenantID := uuid.New()
	var gotSQL string
	tx := &fakeTx{
		queryRowFn: func(_ context.Context, sql string, _ ...any) pgx.Row {
			gotSQL = sql
			return &fakeRow{values: inviteRow(tenantID, "email@x.com", "pending", 1)}
		},
	}
	got, err := NewInvitationRepository(nil).
		FindPendingByEmail(injectTx(context.Background(), tx), tenantID, "email@x.com")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Contains(t, gotSQL, "LOWER(email) = LOWER($2)")
}

func TestInvitationRepo_FindPendingByEmail_NoActiveInvitationReturnsNilNil(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	got, err := NewInvitationRepository(nil).
		FindPendingByEmail(injectTx(context.Background(), tx), uuid.New(), "none@x.com")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestInvitationRepo_FindPendingByEmail_UnrecognizedErrorPassesThrough(t *testing.T) {
	findErr := errors.New("find failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: findErr}
		},
	}
	_, err := NewInvitationRepository(nil).
		FindPendingByEmail(injectTx(context.Background(), tx), uuid.New(), "x@x.com")
	assert.ErrorIs(t, err, findErr)
}

// ── FindPendingByKeycloakUser ────────────────────────────────────────────

func TestInvitationRepo_FindPendingByKeycloakUser_ReturnsRowWhenFound(t *testing.T) {
	tenantID := uuid.New()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: inviteRow(tenantID, "kc@x.com", "pending", 1)}
		},
	}
	got, err := NewInvitationRepository(nil).
		FindPendingByKeycloakUser(injectTx(context.Background(), tx), tenantID, uuid.New())
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestInvitationRepo_FindPendingByKeycloakUser_NoRowsReturnsNilNil(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	got, err := NewInvitationRepository(nil).
		FindPendingByKeycloakUser(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestInvitationRepo_FindPendingByKeycloakUser_UnrecognizedErrorPassesThrough(t *testing.T) {
	findErr := errors.New("find failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: findErr}
		},
	}
	_, err := NewInvitationRepository(nil).
		FindPendingByKeycloakUser(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, findErr)
}

// ── Insert ─────────────────────────────────────────────────────────────

func TestInvitationRepo_Insert_ReturnsCreatedRow(t *testing.T) {
	tenantID := uuid.New()
	var gotSQL string
	tx := &fakeTx{
		queryRowFn: func(_ context.Context, sql string, _ ...any) pgx.Row {
			gotSQL = sql
			return &fakeRow{values: inviteRow(tenantID, "new@x.com", "pending", 1)}
		},
	}
	inv := &domain.PendingInvitation{
		TenantID:            tenantID,
		Email:               "new@x.com",
		FullName:            "New Person",
		InitialTenantRoles:  []domain.TenantRoleCode{domain.RoleTenantAdmin},
		InitialDeptMappings: nil,
	}
	got, err := NewInvitationRepository(nil).Insert(injectTx(context.Background(), tx), inv)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Contains(t, gotSQL, "INSERT INTO pending_invitations")
	assert.NotEqual(t, uuid.Nil, inv.ID, "Insert must assign an ID when the caller left it nil")
}

func TestInvitationRepo_Insert_PreservesCallerSetIDAndStatusAndDeptMappings(t *testing.T) {
	tenantID, id := uuid.New(), uuid.New()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: inviteRow(tenantID, "explicit@x.com", "pending", 1)}
		},
	}
	inv := &domain.PendingInvitation{
		ID:                 id,
		TenantID:           tenantID,
		Email:              "explicit@x.com",
		FullName:           "Explicit Person",
		Status:             domain.InvitePending,
		InitialTenantRoles: []domain.TenantRoleCode{domain.RoleTenantAdmin},
		InitialDeptMappings: []domain.InvitationDeptMapping{
			{DepartmentID: uuid.New(), Level: domain.DeptReviewer},
		},
	}
	got, err := NewInvitationRepository(nil).Insert(injectTx(context.Background(), tx), inv)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, id, inv.ID, "Insert must not overwrite a caller-supplied ID")
}

func TestInvitationRepo_Insert_ScanErrorPassesThrough(t *testing.T) {
	insertErr := errors.New("insert failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: insertErr}
		},
	}
	_, err := NewInvitationRepository(nil).Insert(injectTx(context.Background(), tx), &domain.PendingInvitation{
		TenantID: uuid.New(), Email: "e@x.com",
	})
	assert.ErrorIs(t, err, insertErr)
}

// ── SetKeycloakUserID exec error ─────────────────────────────────────────

func TestInvitationRepo_SetKeycloakUserID_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	err := NewInvitationRepository(nil).
		SetKeycloakUserID(injectTx(context.Background(), tx), uuid.New(), uuid.New(), uuid.New(), 1)
	assert.ErrorIs(t, err, execErr)
}

func TestInvitationRepo_SetKeycloakUserID_ProbeUnrecognizedErrorPassesThrough(t *testing.T) {
	probeErr := errors.New("probe failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: probeErr}
		},
	}
	err := NewInvitationRepository(nil).
		SetKeycloakUserID(injectTx(context.Background(), tx), uuid.New(), uuid.New(), uuid.New(), 1)
	assert.ErrorIs(t, err, probeErr)
}

// ── SetKCCleanupPending exec error ────────────────────────────────────────

func TestInvitationRepo_SetKCCleanupPending_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	err := NewInvitationRepository(nil).
		SetKCCleanupPending(injectTx(context.Background(), tx), uuid.New(), uuid.New(), true, 1)
	assert.ErrorIs(t, err, execErr)
}

func TestInvitationRepo_SetKCCleanupPending_ProbeUnrecognizedErrorPassesThrough(t *testing.T) {
	probeErr := errors.New("probe failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: probeErr}
		},
	}
	err := NewInvitationRepository(nil).
		SetKCCleanupPending(injectTx(context.Background(), tx), uuid.New(), uuid.New(), true, 1)
	assert.ErrorIs(t, err, probeErr)
}

// ── SetStatus (P-31 accept/revoke — biggest residual gap) ───────────────

func TestInvitationRepo_SetStatus_SuccessOnMatchSetsAcceptedAtForAccepted(t *testing.T) {
	tenantID, id := uuid.New(), uuid.New()
	var gotSQL string
	tx := &fakeTx{
		queryRowFn: func(_ context.Context, sql string, _ ...any) pgx.Row {
			gotSQL = sql
			return &fakeRow{values: inviteRow(tenantID, "acc@x.com", "accepted", 2)}
		},
	}
	got, err := NewInvitationRepository(nil).
		SetStatus(injectTx(context.Background(), tx), tenantID, id, domain.InviteAccepted, 1)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Contains(t, gotSQL, "accepted_at = now()")
}

func TestInvitationRepo_SetStatus_SuccessOnMatchNonAcceptedOmitsAcceptedAtClause(t *testing.T) {
	tenantID, id := uuid.New(), uuid.New()
	var gotSQL string
	tx := &fakeTx{
		queryRowFn: func(_ context.Context, sql string, _ ...any) pgx.Row {
			gotSQL = sql
			return &fakeRow{values: inviteRow(tenantID, "rev@x.com", "revoked", 2)}
		},
	}
	got, err := NewInvitationRepository(nil).
		SetStatus(injectTx(context.Background(), tx), tenantID, id, domain.InviteRevoked, 1)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotContains(t, gotSQL, "accepted_at = now()")
}

func TestInvitationRepo_SetStatus_NoRowsProbeUnrecognizedErrorPassesThrough(t *testing.T) {
	probeErr := errors.New("probe failed")
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{err: pgx.ErrNoRows}
			}
			return &fakeRow{err: probeErr}
		},
	}
	_, err := NewInvitationRepository(nil).
		SetStatus(injectTx(context.Background(), tx), uuid.New(), uuid.New(), domain.InviteAccepted, 1)
	assert.ErrorIs(t, err, probeErr)
}

func TestInvitationRepo_SetStatus_ProbeMissingMapsToNotFound(t *testing.T) {
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{err: pgx.ErrNoRows}
			}
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := NewInvitationRepository(nil).
		SetStatus(injectTx(context.Background(), tx), uuid.New(), uuid.New(), domain.InviteAccepted, 1)
	assert.ErrorIs(t, err, domain.ErrInvitationNotFound)
}

func TestInvitationRepo_SetStatus_ProbeTerminalStateMapsToNotFound(t *testing.T) {
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{err: pgx.ErrNoRows}
			}
			return &fakeRow{values: []any{int64(3), "accepted"}}
		},
	}
	_, err := NewInvitationRepository(nil).
		SetStatus(injectTx(context.Background(), tx), uuid.New(), uuid.New(), domain.InviteRevoked, 1)
	assert.ErrorIs(t, err, domain.ErrInvitationNotFound)
}

func TestInvitationRepo_SetStatus_ProbeStillPendingMapsToOptimisticLockConflict(t *testing.T) {
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{err: pgx.ErrNoRows}
			}
			return &fakeRow{values: []any{int64(4), "pending"}}
		},
	}
	_, err := NewInvitationRepository(nil).
		SetStatus(injectTx(context.Background(), tx), uuid.New(), uuid.New(), domain.InviteAccepted, 1)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
	assert.EqualValues(t, 4, de.Details["record_version"])
}

func TestInvitationRepo_SetStatus_UnrecognizedScanErrorPassesThrough(t *testing.T) {
	scanErr := errors.New("scan failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: scanErr}
		},
	}
	_, err := NewInvitationRepository(nil).
		SetStatus(injectTx(context.Background(), tx), uuid.New(), uuid.New(), domain.InviteAccepted, 1)
	assert.ErrorIs(t, err, scanErr)
}

// ── ListExpiring error paths ─────────────────────────────────────────────

func TestInvitationRepo_ListExpiring_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewInvitationRepository(nil).ListExpiring(injectTx(context.Background(), tx), time.Now(), 10)
	assert.ErrorIs(t, err, queryErr)
}

func TestInvitationRepo_ListExpiring_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil
		},
	}
	_, err := NewInvitationRepository(nil).ListExpiring(injectTx(context.Background(), tx), time.Now(), 10)
	assert.Error(t, err)
}

func TestInvitationRepo_ListExpiring_IterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("iteration failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewInvitationRepository(nil).ListExpiring(injectTx(context.Background(), tx), time.Now(), 10)
	assert.ErrorIs(t, err, iterErr)
}

// ── MostRecentCreatedAt (PI-11 cooldown) ─────────────────────────────────

func TestInvitationRepo_MostRecentCreatedAt_ReturnsScannedTime(t *testing.T) {
	want := time.Now()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: []any{want}}
		},
	}
	got, err := NewInvitationRepository(nil).
		MostRecentCreatedAt(injectTx(context.Background(), tx), uuid.New(), "cool@x.com")
	require.NoError(t, err)
	assert.WithinDuration(t, want, got, time.Second)
}

func TestInvitationRepo_MostRecentCreatedAt_NoRowsReturnsZeroTimeNoError(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	got, err := NewInvitationRepository(nil).
		MostRecentCreatedAt(injectTx(context.Background(), tx), uuid.New(), "none@x.com")
	require.NoError(t, err)
	assert.True(t, got.IsZero())
}

func TestInvitationRepo_MostRecentCreatedAt_UnrecognizedErrorPassesThrough(t *testing.T) {
	scanErr := errors.New("scan failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: scanErr}
		},
	}
	_, err := NewInvitationRepository(nil).
		MostRecentCreatedAt(injectTx(context.Background(), tx), uuid.New(), "x@x.com")
	assert.ErrorIs(t, err, scanErr)
}

// ── CountCreatedInWindow (PI-12 per-tenant rate check) ───────────────────

func TestInvitationRepo_CountCreatedInWindow_ReturnsScannedCount(t *testing.T) {
	var gotArgs []any
	tx := &fakeTx{
		queryRowFn: func(_ context.Context, _ string, args ...any) pgx.Row {
			gotArgs = args
			return &fakeRow{values: []any{4}}
		},
	}
	since := time.Now().Add(-1 * time.Hour)
	got, err := NewInvitationRepository(nil).
		CountCreatedInWindow(injectTx(context.Background(), tx), uuid.New(), since)
	require.NoError(t, err)
	assert.Equal(t, 4, got)
	require.Len(t, gotArgs, 2)
	assert.WithinDuration(t, since, gotArgs[1].(time.Time), time.Second)
}

func TestInvitationRepo_CountCreatedInWindow_ScanErrorPassesThrough(t *testing.T) {
	scanErr := errors.New("scan failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: scanErr}
		},
	}
	_, err := NewInvitationRepository(nil).
		CountCreatedInWindow(injectTx(context.Background(), tx), uuid.New(), time.Now())
	assert.ErrorIs(t, err, scanErr)
}

// ── ExpireOverdue (invitation-expiry reconciler) ─────────────────────────

func TestInvitationRepo_ExpireOverdue_ReturnsRowsAffected(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 6"), nil
		},
	}
	n, err := NewInvitationRepository(nil).ExpireOverdue(injectTx(context.Background(), tx), 50)
	require.NoError(t, err)
	assert.Equal(t, 6, n)
}

func TestInvitationRepo_ExpireOverdue_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	_, err := NewInvitationRepository(nil).ExpireOverdue(injectTx(context.Background(), tx), 50)
	assert.ErrorIs(t, err, execErr)
}

// ── scanInvitation branches ───────────────────────────────────────────────

func TestScanInvitation_NilDeptMappingsJSONDefaultsToEmptySlice(t *testing.T) {
	row := &fakeRow{values: []any{
		uuid.New(), uuid.New(), "x@x.com", "X",
		[]string{"member"}, []byte(nil),
		uuid.New(), (*uuid.UUID)(nil), "pending", time.Now(), (*time.Time)(nil),
		false, int64(1), time.Now(), time.Now(),
	}}
	got, err := scanInvitation(row)
	require.NoError(t, err)
	require.NotNil(t, got.InitialDeptMappings)
	assert.Empty(t, got.InitialDeptMappings)
}

func TestScanInvitation_InvalidDeptMappingsJSONReturnsError(t *testing.T) {
	row := &fakeRow{values: []any{
		uuid.New(), uuid.New(), "x@x.com", "X",
		[]string{"member"}, []byte("{not-json"),
		uuid.New(), (*uuid.UUID)(nil), "pending", time.Now(), (*time.Time)(nil),
		false, int64(1), time.Now(), time.Now(),
	}}
	_, err := scanInvitation(row)
	require.Error(t, err)
}

func TestScanInvitation_ScanErrorPassesThrough(t *testing.T) {
	scanErr := errors.New("scan failed")
	row := &fakeRow{err: scanErr}
	_, err := scanInvitation(row)
	assert.ErrorIs(t, err, scanErr)
}
