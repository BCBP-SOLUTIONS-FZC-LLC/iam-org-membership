package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authzHeaderRow matches the first QueryRow's Scan order: mStatus, tStatus,
// tPlan, tLocale, mfaFresh, localAccountsEnabled, tenantFeatureFlagsJSON.
func authzHeaderRow(featureFlagsJSON []byte) []any {
	return []any{"active", "active", "pro", "en-US", 300, true, featureFlagsJSON}
}

func TestAuthZRepo_FindMembershipProjection_NoMembershipRowReturnsNilNil(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgx.ErrNoRows}
		},
	}
	got, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.Nil(t, got, "caller (AuthZService) maps a nil result to 404")
}

func TestAuthZRepo_FindMembershipProjection_HeaderQueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("dial tcp: connection refused")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: queryErr}
		},
	}
	_, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, queryErr)
}

func TestAuthZRepo_FindMembershipProjection_RolesQueryErrorPassesThrough(t *testing.T) {
	queryErr := errors.New("roles query failed")
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzHeaderRow(nil)}
		},
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, queryErr
		},
	}
	_, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, queryErr)
}

func TestAuthZRepo_FindMembershipProjection_RolesScanErrorPassesThrough(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzHeaderRow(nil)}
		},
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			// Wrong arity forces scanInto to error inside rows.Scan.
			return &fakeRows{scripted: [][]any{{uuid.New(), "extra"}}}, nil
		},
	}
	_, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.Error(t, err)
}

func TestAuthZRepo_FindMembershipProjection_RolesIterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("mid-stream reset")
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzHeaderRow(nil)}
		},
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			callCount++
			if callCount == 1 {
				return &fakeRows{iterErr: iterErr}, nil
			}
			return &fakeRows{}, nil
		},
	}
	_, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, iterErr)
}

func TestAuthZRepo_FindMembershipProjection_DeptQueryErrorPassesThrough(t *testing.T) {
	deptErr := errors.New("dept query failed")
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzHeaderRow(nil)}
		},
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			callCount++
			if callCount == 1 {
				// roles query — succeeds with no rows.
				return &fakeRows{}, nil
			}
			return nil, deptErr
		},
	}
	_, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, deptErr)
}

func TestAuthZRepo_FindMembershipProjection_DeptScanErrorPassesThrough(t *testing.T) {
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzHeaderRow(nil)}
		},
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			callCount++
			if callCount == 1 {
				return &fakeRows{}, nil
			}
			// Wrong arity forces scanInto to error.
			return &fakeRows{scripted: [][]any{{uuid.New()}}}, nil
		},
	}
	_, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.Error(t, err)
}

func TestAuthZRepo_FindMembershipProjection_DeptIterationErrorPassesThrough(t *testing.T) {
	iterErr := errors.New("dept stream reset")
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzHeaderRow(nil)}
		},
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			callCount++
			if callCount == 1 {
				return &fakeRows{}, nil
			}
			return &fakeRows{iterErr: iterErr}, nil
		},
	}
	_, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.ErrorIs(t, err, iterErr)
}

func TestAuthZRepo_FindMembershipProjection_InvalidFeatureFlagsJSONReturnsError(t *testing.T) {
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzHeaderRow([]byte("{not-json"))}
		},
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			callCount++
			return &fakeRows{}, nil
		},
	}
	_, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.Error(t, err)
	assert.Equal(t, 2, callCount, "both roles and dept queries run before the flags unmarshal")
}

func TestAuthZRepo_FindMembershipProjection_HappyPathFullProjection(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	deptID := uuid.New()
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzHeaderRow([]byte(`{"beta":true}`))}
		},
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			callCount++
			if callCount == 1 {
				return &fakeRows{scripted: [][]any{{"tenant_admin"}}}, nil
			}
			return &fakeRows{scripted: [][]any{{deptID, "reviewer"}}}, nil
		},
	}
	got, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), tenantID, userID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, domain.MembershipStatus("active"), got.MembershipStatus)
	assert.Equal(t, domain.TenantPlan("pro"), got.TenantPlan)
	assert.Equal(t, domain.SubscriptionStatus("active"), got.SubscriptionStatus)
	assert.Equal(t, "en-US", got.Locale)
	assert.Equal(t, 300, got.MFAFreshnessSeconds)
	assert.True(t, got.LocalAccountsEnabled)
	assert.Equal(t, true, got.TenantFeatureFlags["beta"])
	require.Len(t, got.Roles, 1)
	assert.Equal(t, domain.TenantRoleCode("tenant_admin"), got.Roles[0])
	require.Len(t, got.Departments, 1)
	assert.Equal(t, deptID, got.Departments[0].DepartmentID)
	assert.Equal(t, domain.DeptRole("reviewer"), got.Departments[0].RoleLevel)
}

func TestAuthZRepo_FindMembershipProjection_NoFeatureFlagsDefaultsToEmptyMap(t *testing.T) {
	callCount := 0
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzHeaderRow(nil)}
		},
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			callCount++
			return &fakeRows{}, nil
		},
	}
	got, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Empty(t, got.TenantFeatureFlags)
	assert.Empty(t, got.Departments, "no dept rows -> empty (non-nil) slice")
}
