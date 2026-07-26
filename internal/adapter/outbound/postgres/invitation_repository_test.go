package postgres

import (
	"context"
	"encoding/json"
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
