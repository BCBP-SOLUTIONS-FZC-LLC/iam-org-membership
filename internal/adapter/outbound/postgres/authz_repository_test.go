package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authzSingleRow returns the 10-column row that FindMembershipProjection now
// scans in a single LEFT JOIN query (PERF-1 refactor). Column order matches
// the Scan call in authz_repository.go:
//
//	mStatus, tStatus, tPlan, tLocale, mfaFresh, localAccountsEnabled,
//	residency_mode (CD-37), tenantFeatureFlagsJSON, rolesJSON, deptsJSON
func authzSingleRow(featureFlagsJSON []byte, rolesJSON []byte, deptsJSON []byte) []any {
	if featureFlagsJSON == nil {
		featureFlagsJSON = []byte("null")
	}
	if rolesJSON == nil {
		rolesJSON = []byte("[]")
	}
	if deptsJSON == nil {
		deptsJSON = []byte("[]")
	}
	return []any{
		"active",         // mStatus
		"active",         // tStatus
		"pro",            // tPlan
		"en-US",          // tLocale
		300,              // mfaFresh
		true,             // localAccountsEnabled
		"standard",       // residency_mode (CD-37)
		featureFlagsJSON, // tenantFeatureFlagsJSON
		rolesJSON,        // roles as JSON array e.g. ["tenant_admin"]
		deptsJSON,        // depts as JSON array e.g. [{"department_id":"...","role_level":"reviewer"}]
	}
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

func TestAuthZRepo_FindMembershipProjection_QueryErrorPassesThrough(t *testing.T) {
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

func TestAuthZRepo_FindMembershipProjection_InvalidRolesJSONReturnsError(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzSingleRow(nil, []byte("{not-json-array"), nil)}
		},
	}
	_, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.Error(t, err, "malformed roles JSON must propagate as error")
}

func TestAuthZRepo_FindMembershipProjection_InvalidDeptsJSONReturnsError(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzSingleRow(nil, nil, []byte("{not-json-array"))}
		},
	}
	_, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.Error(t, err, "malformed depts JSON must propagate as error")
}

func TestAuthZRepo_FindMembershipProjection_InvalidFeatureFlagsJSONReturnsError(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzSingleRow([]byte("{not-json"), nil, nil)}
		},
	}
	_, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	assert.Error(t, err)
}

func TestAuthZRepo_FindMembershipProjection_HappyPathFullProjection(t *testing.T) {
	tenantID, userID := uuid.New(), uuid.New()
	deptID := uuid.New()

	rolesJSON := []byte(`["tenant_admin"]`)
	deptsJSON := []byte(fmt.Sprintf(`[{"department_id":%q,"role_level":"reviewer"}]`, deptID))

	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzSingleRow([]byte(`{"beta":true}`), rolesJSON, deptsJSON)}
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
	assert.Equal(t, domain.ResidencyModeStandard, got.ResidencyMode, "CD-37: residency_mode must be projected")
	assert.Equal(t, true, got.TenantFeatureFlags["beta"])
	require.Len(t, got.Roles, 1)
	assert.Equal(t, domain.TenantRoleCode("tenant_admin"), got.Roles[0])
	require.Len(t, got.Departments, 1)
	assert.Equal(t, deptID, got.Departments[0].DepartmentID)
	assert.Equal(t, domain.DeptRole("reviewer"), got.Departments[0].RoleLevel)
}

func TestAuthZRepo_FindMembershipProjection_NoFeatureFlagsDefaultsToEmptyMap(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			// null featureFlags, empty roles and depts arrays
			return &fakeRow{values: authzSingleRow(nil, nil, nil)}
		},
	}
	got, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Empty(t, got.TenantFeatureFlags)
	assert.Empty(t, got.Departments, "no dept rows -> empty (non-nil) slice")
	assert.Empty(t, got.Roles, "no role rows -> empty (non-nil) slice")
}

func TestAuthZRepo_FindMembershipProjection_MultipleRolesAndDepts(t *testing.T) {
	dept1, dept2 := uuid.New(), uuid.New()
	rolesJSON := []byte(`["tender_admin","tenant_owner"]`)
	deptsJSON := []byte(fmt.Sprintf(
		`[{"department_id":%q,"role_level":"preparator"},{"department_id":%q,"role_level":"approver"}]`,
		dept1, dept2,
	))

	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{values: authzSingleRow(nil, rolesJSON, deptsJSON)}
		},
	}
	got, err := NewAuthZRepository(nil).
		FindMembershipProjection(injectTx(context.Background(), tx), uuid.New(), uuid.New())
	require.NoError(t, err)
	require.Len(t, got.Roles, 2)
	require.Len(t, got.Departments, 2)
	assert.Equal(t, domain.DeptRole("preparator"), got.Departments[0].RoleLevel)
	assert.Equal(t, domain.DeptRole("approver"), got.Departments[1].RoleLevel)
}
