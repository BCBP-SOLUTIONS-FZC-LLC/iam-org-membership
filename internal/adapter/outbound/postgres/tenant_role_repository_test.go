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

func TestTenantRoleRepo_ListByRole_ConnectivityErrorMapsToDependencyUnavailable(t *testing.T) {
	// wrapConnErr in db.go converts unknown (non-pgconn, non-context)
	// errors to ErrDependencyUnavailable so handlers get a clean 503.
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return nil, errors.New("dial tcp: connection refused")
		},
	}
	repo := NewTenantRoleRepository(nil)
	_, err := repo.ListByRole(injectTx(context.Background(), tx), uuid.New(), domain.RoleTenantAdmin)
	assert.ErrorIs(t, err, domain.ErrDependencyUnavailable)
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

func TestTenantRoleRepo_ListByRole_RowsIteratorErrorMapsToDependencyUnavailable(t *testing.T) {
	// A rows.Err() from a mid-stream connectivity blip flows through the
	// same wrapConnErr mapping.
	tx := &fakeTx{
		queryFn: func(context.Context, string, ...any) (pgx.Rows, error) {
			return &fakeRows{iterErr: errors.New("network reset mid-stream")}, nil
		},
	}
	repo := NewTenantRoleRepository(nil)
	_, err := repo.ListByRole(injectTx(context.Background(), tx), uuid.New(), domain.RoleTenantAdmin)
	assert.ErrorIs(t, err, domain.ErrDependencyUnavailable)
}
