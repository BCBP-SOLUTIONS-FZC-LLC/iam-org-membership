package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── FindByIDIncludingDeleted (I-2 RP cleanup / reconcilers) ────────────

func TestTenantRepo_FindByIDIncludingDeleted_ReturnsRow(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: tenantRow(nil, nil)}
		},
	}
	got, err := NewTenantRepository(nil).
		FindByIDIncludingDeleted(injectTx(context.Background(), tx), uuid.New())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "acme", got.Slug)
}

func TestTenantRepo_FindByIDIncludingDeleted_NoRowsMapsToTenantNotFound(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := NewTenantRepository(nil).
		FindByIDIncludingDeleted(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, domain.ErrTenantNotFound)
}

func TestTenantRepo_FindByIDIncludingDeleted_UnrecognizedErrorPassesThrough(t *testing.T) {
	scanErr := errors.New("scan failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: scanErr}
		},
	}
	_, err := NewTenantRepository(nil).
		FindByIDIncludingDeleted(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, scanErr)
}

// ── SetRealmSyncPending (T-15/§16 A58) ──────────────────────────────────

func TestTenantRepo_SetRealmSyncPending_SuccessWhenRowAffected(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	err := NewTenantRepository(nil).
		SetRealmSyncPending(injectTx(context.Background(), tx), uuid.New())
	assert.NoError(t, err)
}

func TestTenantRepo_SetRealmSyncPending_NoRowsMapsToTenantNotFound(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
	}
	err := NewTenantRepository(nil).
		SetRealmSyncPending(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, domain.ErrTenantNotFound)
}

func TestTenantRepo_SetRealmSyncPending_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	err := NewTenantRepository(nil).
		SetRealmSyncPending(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, execErr)
}

// ── SetFeatureFlags (O-4) ────────────────────────────────────────────────

func TestTenantRepo_SetFeatureFlags_SuccessWhenRowAffected(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	err := NewTenantRepository(nil).
		SetFeatureFlags(injectTx(context.Background(), tx), uuid.New(), []byte(`{"beta":true}`), 1)
	assert.NoError(t, err)
}

func TestTenantRepo_SetFeatureFlags_NoRowsProbeConflictWhenVersionDiffers(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: []any{int64(3)}}
		},
	}
	err := NewTenantRepository(nil).
		SetFeatureFlags(injectTx(context.Background(), tx), uuid.New(), []byte(`{}`), 1)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
	assert.EqualValues(t, 3, de.Details["record_version"])
}

func TestTenantRepo_SetFeatureFlags_NoRowsProbeMissingMapsToTenantNotFound(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	err := NewTenantRepository(nil).
		SetFeatureFlags(injectTx(context.Background(), tx), uuid.New(), []byte(`{}`), 1)
	assert.ErrorIs(t, err, domain.ErrTenantNotFound)
}

func TestTenantRepo_SetFeatureFlags_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	err := NewTenantRepository(nil).
		SetFeatureFlags(injectTx(context.Background(), tx), uuid.New(), []byte(`{}`), 1)
	assert.ErrorIs(t, err, execErr)
}

// ── LockSeatOccupancy (SEAT-1 preflight FOR UPDATE) ─────────────────────

func TestTenantRepo_LockSeatOccupancy_HappyPath(t *testing.T) {
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			switch callCount {
			case 1:
				return &fakeRow{values: []any{10, (*time.Time)(nil)}}
			case 2:
				return &fakeRow{values: []any{7}}
			default:
				return &fakeRow{values: []any{2}}
			}
		},
	}
	occ, err := NewTenantRepository(nil).
		LockSeatOccupancy(injectTx(context.Background(), tx), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, 10, occ.LicensedSeats)
	assert.Equal(t, 7, occ.Active)
	assert.Equal(t, 2, occ.Pending)
}

func TestTenantRepo_LockSeatOccupancy_FirstQueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("lock failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: queryErr}
		},
	}
	_, err := NewTenantRepository(nil).
		LockSeatOccupancy(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, queryErr)
}

func TestTenantRepo_LockSeatOccupancy_SecondQueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("active count failed")
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{values: []any{10, (*time.Time)(nil)}}
			}
			return &fakeRow{err: queryErr}
		},
	}
	_, err := NewTenantRepository(nil).
		LockSeatOccupancy(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, queryErr)
}

func TestTenantRepo_LockSeatOccupancy_ThirdQueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("pending count failed")
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			switch callCount {
			case 1:
				return &fakeRow{values: []any{10, (*time.Time)(nil)}}
			case 2:
				return &fakeRow{values: []any{7}}
			default:
				return &fakeRow{err: queryErr}
			}
		},
	}
	_, err := NewTenantRepository(nil).
		LockSeatOccupancy(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, queryErr)
}

// ── optimisticConflictOrNotFound ────────────────────────────────────────

func TestTenantRepo_optimisticConflictOrNotFound_ReturnsConflictWithDetails(t *testing.T) {
	now := time.Now()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: []any{int64(4), now}}
		},
	}
	err := (&TenantRepository{}).optimisticConflictOrNotFound(context.Background(), tx, uuid.New())
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
	assert.EqualValues(t, 4, de.Details["record_version"])
}

func TestTenantRepo_optimisticConflictOrNotFound_NoRowsMapsToTenantNotFound(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	err := (&TenantRepository{}).optimisticConflictOrNotFound(context.Background(), tx, uuid.New())
	assert.ErrorIs(t, err, domain.ErrTenantNotFound)
}

func TestTenantRepo_optimisticConflictOrNotFound_UnrecognizedErrorPassesThrough(t *testing.T) {
	probeErr := errors.New("probe failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: probeErr}
		},
	}
	err := (&TenantRepository{}).optimisticConflictOrNotFound(context.Background(), tx, uuid.New())
	assert.ErrorIs(t, err, probeErr)
}

// Update falls through to optimisticConflictOrNotFound on a no-rows UPDATE.
func TestTenantRepo_Update_NoRowsFallsThroughToOptimisticConflict(t *testing.T) {
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{err: pgx.ErrNoRows}
			}
			return &fakeRow{values: []any{int64(2), time.Now()}}
		},
	}
	name := "New Name"
	_, err := NewTenantRepository(nil).
		Update(injectTx(context.Background(), tx), uuid.New(), &domain.TenantPatch{Name: &name, RecordVersion: 1})
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
}

func TestTenantRepo_Update_NilPatchReturnsValidationError(t *testing.T) {
	_, err := NewTenantRepository(nil).Update(context.Background(), uuid.New(), nil)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
}

func TestTenantRepo_Update_NoSetFieldsFallsThroughToFindByID(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: tenantRow(nil, nil)}
		},
	}
	got, err := NewTenantRepository(nil).
		Update(injectTx(context.Background(), tx), uuid.New(), &domain.TenantPatch{RecordVersion: 1})
	require.NoError(t, err)
	assert.Equal(t, "acme", got.Slug)
}

// ── execLifecyclePatch (EVT-16 relay / T-14 / RP-6 / SEAT-5 projections) ─

func TestExecLifecyclePatch_AllKnownOpsBuildValidSQL(t *testing.T) {
	cases := []port.TenantLifecyclePatch{
		{Op: port.LifecycleSetRealm, RealmID: "r1", RealmType: "shared", KeycloakShard: "s1"},
		{Op: port.LifecycleActivatePaid, Plan: domain.PlanPro},
		{Op: port.LifecycleSetStatusClearSuspension, Status: domain.StatusActive},
		{Op: port.LifecycleTrialReactivate, TrialDurationDays: 14},
		{Op: port.LifecycleSuspendBillingLapse},
		{Op: port.LifecycleSuspendOperator},
		{Op: port.LifecycleOffboard},
		{Op: port.LifecycleSetPlan, Plan: domain.PlanEnterprise},
		{Op: port.LifecycleCancel},
		{Op: port.LifecycleReactivatePaid},
		{Op: port.LifecycleReactivateTrial},
		{Op: port.LifecycleSetLicensedSeats, LicensedSeats: 25},
	}
	for _, patch := range cases {
		tx := &fakeTx{
			execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
				return pgconn.NewCommandTag("UPDATE 1"), nil
			},
		}
		rows, err := execLifecyclePatch(context.Background(), tx, uuid.New(), patch)
		require.NoError(t, err)
		assert.EqualValues(t, 1, rows)
	}
}

func TestExecLifecyclePatch_UnknownOpReturnsError(t *testing.T) {
	tx := &fakeTx{}
	_, err := execLifecyclePatch(context.Background(), tx, uuid.New(), port.TenantLifecyclePatch{Op: 999})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown tenant lifecycle op")
}

func TestExecLifecyclePatch_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	_, err := execLifecyclePatch(context.Background(), tx, uuid.New(), port.TenantLifecyclePatch{Op: port.LifecycleCancel})
	assert.ErrorIs(t, err, execErr)
}

func TestTenantRepo_ApplyLifecyclePatch_DelegatesToExecLifecyclePatch(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	rows, err := NewTenantRepository(nil).
		ApplyLifecyclePatch(injectTx(context.Background(), tx), uuid.New(), port.TenantLifecyclePatch{Op: port.LifecycleCancel})
	require.NoError(t, err)
	assert.EqualValues(t, 1, rows)
}

// ── LockForProjection (EVT-14 high-water lock) ──────────────────────────

func TestTenantRepo_LockForProjection_HappyPath(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: []any{"active", "pro", (*time.Time)(nil)}}
		},
	}
	got, err := NewTenantRepository(nil).
		LockForProjection(injectTx(context.Background(), tx), uuid.New())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.SubscriptionStatus("active"), got.Status)
}

func TestTenantRepo_LockForProjection_NoRowsReturnsNilNil(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	got, err := NewTenantRepository(nil).
		LockForProjection(injectTx(context.Background(), tx), uuid.New())
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestTenantRepo_LockForProjection_UnrecognizedErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("lock failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: queryErr}
		},
	}
	_, err := NewTenantRepository(nil).
		LockForProjection(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, queryErr)
}

// ── SetRealmFields ───────────────────────────────────────────────────────

func TestTenantRepo_SetRealmFields_SuccessWhenRowAffected(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	err := NewTenantRepository(nil).
		SetRealmFields(injectTx(context.Background(), tx), uuid.New(), "r1", domain.RealmType("shared"), "s1", 1)
	assert.NoError(t, err)
}

func TestTenantRepo_SetRealmFields_NoRowsProbeConflict(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: []any{int64(6)}}
		},
	}
	err := NewTenantRepository(nil).
		SetRealmFields(injectTx(context.Background(), tx), uuid.New(), "r1", domain.RealmType("shared"), "s1", 1)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "optimistic_lock_conflict", de.Code)
}

func TestTenantRepo_SetRealmFields_NoRowsProbeMissingMapsToNotFound(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	err := NewTenantRepository(nil).
		SetRealmFields(injectTx(context.Background(), tx), uuid.New(), "r1", domain.RealmType("shared"), "s1", 1)
	assert.ErrorIs(t, err, domain.ErrTenantNotFound)
}

// ── MarkOwnerlessIfUnset / ClearOwnerlessSince ──────────────────────────

func TestTenantRepo_MarkOwnerlessIfUnset_FlipsWhenRowAffected(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	flipped, err := NewTenantRepository(nil).
		MarkOwnerlessIfUnset(injectTx(context.Background(), tx), uuid.New())
	require.NoError(t, err)
	assert.True(t, flipped)
}

func TestTenantRepo_MarkOwnerlessIfUnset_NoOpWhenAlreadySet(t *testing.T) {
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		},
	}
	flipped, err := NewTenantRepository(nil).
		MarkOwnerlessIfUnset(injectTx(context.Background(), tx), uuid.New())
	require.NoError(t, err)
	assert.False(t, flipped)
}

func TestTenantRepo_ClearOwnerlessSince_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	err := NewTenantRepository(nil).ClearOwnerlessSince(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, execErr)
}

// ── LockByID / LicensedSeatsForUpdate / SetOverageSince / SetLastEventAt ─

func TestTenantRepo_LockByID_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("lock failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	err := NewTenantRepository(nil).LockByID(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, execErr)
}

func TestTenantRepo_LicensedSeatsForUpdate_ReturnsCount(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: []any{15}}
		},
	}
	n, err := NewTenantRepository(nil).
		LicensedSeatsForUpdate(injectTx(context.Background(), tx), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, 15, n)
}

func TestTenantRepo_SetOverageSince_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	err := NewTenantRepository(nil).
		SetOverageSince(injectTx(context.Background(), tx), uuid.New(), nil)
	assert.ErrorIs(t, err, execErr)
}

func TestTenantRepo_SetLastEventAt_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	err := NewTenantRepository(nil).
		SetLastEventAt(injectTx(context.Background(), tx), uuid.New(), time.Now())
	assert.ErrorIs(t, err, execErr)
}

// ── WipeTenantChildren (§15.5 GDPR wipe) ────────────────────────────────

func TestTenantRepo_WipeTenantChildren_DeletesEachTable(t *testing.T) {
	var tables []string
	tx := &fakeTx{
		execFn: func(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
			tables = append(tables, sql)
			return pgconn.NewCommandTag("DELETE 0"), nil
		},
	}
	err := NewTenantRepository(nil).WipeTenantChildren(injectTx(context.Background(), tx), uuid.New())
	require.NoError(t, err)
	assert.Len(t, tables, len(gdprWipeTables))
}

func TestTenantRepo_WipeTenantChildren_ErrorWrapsTableName(t *testing.T) {
	deleteErr := errors.New("delete failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, deleteErr
		},
	}
	err := NewTenantRepository(nil).WipeTenantChildren(injectTx(context.Background(), tx), uuid.New())
	require.Error(t, err)
	assert.ErrorIs(t, err, deleteErr)
	assert.Contains(t, err.Error(), "GDPR wipe")
}

// ── ListSubscriptionLapses (I-16) ───────────────────────────────────────

func TestTenantRepo_ListSubscriptionLapses_ReturnsRows(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{tenantRow(nil, nil)}}, nil
		},
	}
	got, err := NewTenantRepository(nil).
		ListSubscriptionLapses(injectTx(context.Background(), tx), 30)
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestTenantRepo_ListSubscriptionLapses_QueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("query failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewTenantRepository(nil).ListSubscriptionLapses(injectTx(context.Background(), tx), 30)
	assert.ErrorIs(t, err, queryErr)
}

func TestTenantRepo_ListSubscriptionLapses_IterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("iteration failed")
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewTenantRepository(nil).ListSubscriptionLapses(injectTx(context.Background(), tx), 30)
	assert.ErrorIs(t, err, iterErr)
}

func TestTenantRepo_ListSubscriptionLapses_ScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil
		},
	}
	_, err := NewTenantRepository(nil).ListSubscriptionLapses(injectTx(context.Background(), tx), 30)
	assert.Error(t, err)
}

// ── FindByID ─────────────────────────────────────────────────────────────

func TestTenantRepo_FindByID_ReturnsRow(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: tenantRow(nil, nil)}
		},
	}
	got, err := NewTenantRepository(nil).FindByID(injectTx(context.Background(), tx), uuid.New())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "acme", got.Slug)
}

func TestTenantRepo_FindByID_NoRowsMapsToTenantNotFound(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := NewTenantRepository(nil).FindByID(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, domain.ErrTenantNotFound)
}

func TestTenantRepo_FindByID_UnrecognizedErrorPassesThrough(t *testing.T) {
	findErr := errors.New("find failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: findErr}
		},
	}
	_, err := NewTenantRepository(nil).FindByID(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, findErr)
}

// ── Update — additional branches ────────────────────────────────────────

func TestTenantRepo_Update_SuccessOnMatch(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: tenantRow(nil, nil)}
		},
	}
	name := "New Name"
	locale := "fr-FR"
	enabled := true
	mfaFreshness := 200
	got, err := NewTenantRepository(nil).Update(injectTx(context.Background(), tx), uuid.New(), &domain.TenantPatch{
		Name: &name, DefaultLocale: &locale, LocalAccountsEnabled: &enabled,
		MFAFreshnessSeconds: &mfaFreshness, RecordVersion: 1,
	})
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestTenantRepo_Update_UnrecognizedScanErrorPassesThrough(t *testing.T) {
	updateErr := errors.New("update failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: updateErr}
		},
	}
	name := "X"
	_, err := NewTenantRepository(nil).Update(injectTx(context.Background(), tx), uuid.New(), &domain.TenantPatch{
		Name: &name, RecordVersion: 1,
	})
	assert.ErrorIs(t, err, updateErr)
}

// ── Insert — additional branches ────────────────────────────────────────

func TestTenantRepo_Insert_NilTenantReturnsValidationError(t *testing.T) {
	_, created, err := NewTenantRepository(nil).Insert(context.Background(), nil)
	var de *domain.DomainError
	require.ErrorAs(t, err, &de)
	assert.Equal(t, "validation_error", de.Code)
	assert.False(t, created)
}

func TestTenantRepo_Insert_ReturnsCreatedRowOnFreshInsert(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: tenantRow(nil, nil)}
		},
	}
	got, created, err := NewTenantRepository(nil).Insert(injectTx(context.Background(), tx), &domain.Tenant{
		Slug: "acme", Name: "Acme", Plan: domain.PlanStarter, Status: domain.StatusTrial,
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, created)
}

func TestTenantRepo_Insert_PreservesCallerSetIDAndFeatureFlags(t *testing.T) {
	id := uuid.New()
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: tenantRow([]byte(`{"beta":true}`), nil)}
		},
	}
	tenant := &domain.Tenant{
		ID: id, Slug: "acme", Name: "Acme", Plan: domain.PlanStarter, Status: domain.StatusTrial,
		FeatureFlags: map[string]any{"beta": true},
	}
	got, created, err := NewTenantRepository(nil).Insert(injectTx(context.Background(), tx), tenant)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, created)
	assert.Equal(t, id, tenant.ID, "Insert must not overwrite a caller-supplied ID")
}

func TestTenantRepo_Insert_ConflictAbsorbedFetchesExistingRow(t *testing.T) {
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{err: pgx.ErrNoRows} // ON CONFLICT (id) DO NOTHING absorbed
			}
			return &fakeRow{values: tenantRow(nil, nil)} // existing row fetch
		},
	}
	id := uuid.New()
	got, created, err := NewTenantRepository(nil).Insert(injectTx(context.Background(), tx), &domain.Tenant{
		ID: id, Slug: "acme", Name: "Acme", Plan: domain.PlanStarter, Status: domain.StatusTrial,
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.False(t, created, "idempotent replay must not report wasCreated")
}

func TestTenantRepo_Insert_ConflictAbsorbedExistingFetchErrorPassesThrough(t *testing.T) {
	existingErr := errors.New("existing fetch failed")
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			callCount++
			if callCount == 1 {
				return &fakeRow{err: pgx.ErrNoRows}
			}
			return &fakeRow{err: existingErr}
		},
	}
	_, _, err := NewTenantRepository(nil).Insert(injectTx(context.Background(), tx), &domain.Tenant{
		Slug: "acme", Name: "Acme", Plan: domain.PlanStarter, Status: domain.StatusTrial,
	})
	assert.ErrorIs(t, err, existingErr)
}

func TestTenantRepo_Insert_GenericScanErrorPassesThrough(t *testing.T) {
	scanErr := errors.New("scan failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: scanErr}
		},
	}
	_, _, err := NewTenantRepository(nil).Insert(injectTx(context.Background(), tx), &domain.Tenant{
		Slug: "acme", Name: "Acme", Plan: domain.PlanStarter, Status: domain.StatusTrial,
	})
	assert.ErrorIs(t, err, scanErr)
}

// ── MarkOwnerlessIfUnset — exec error path ───────────────────────────────

func TestTenantRepo_MarkOwnerlessIfUnset_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	_, err := NewTenantRepository(nil).
		MarkOwnerlessIfUnset(injectTx(context.Background(), tx), uuid.New())
	assert.ErrorIs(t, err, execErr)
}

// ── SetRealmFields — exec error path ─────────────────────────────────────

func TestTenantRepo_SetRealmFields_ExecErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	err := NewTenantRepository(nil).
		SetRealmFields(injectTx(context.Background(), tx), uuid.New(), "r1", domain.RealmType("shared"), "s1", 1)
	assert.ErrorIs(t, err, execErr)
}

// ── ApplyLifecyclePatch — error path ─────────────────────────────────────

func TestTenantRepo_ApplyLifecyclePatch_ErrorPassesThrough(t *testing.T) {
	execErr := errors.New("exec failed")
	tx := &fakeTx{
		execFn: func(context.Context, string, ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, execErr
		},
	}
	_, err := NewTenantRepository(nil).
		ApplyLifecyclePatch(injectTx(context.Background(), tx), uuid.New(), port.TenantLifecyclePatch{Op: port.LifecycleCancel})
	assert.ErrorIs(t, err, execErr)
}
