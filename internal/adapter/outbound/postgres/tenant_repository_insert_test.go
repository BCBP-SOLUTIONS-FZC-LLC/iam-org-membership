package postgres

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTenantRepository_Insert_SlugUniqueViolation(t *testing.T) {
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: &pgconn.PgError{Code: "23505", ConstraintName: "uq_tenants_slug"}}
		},
	}
	repo := NewTenantRepository(nil)
	_, created, err := repo.Insert(injectTx(context.Background(), tx), &domain.Tenant{
		Slug: "taken", Name: "T", Plan: domain.PlanStarter, Status: domain.StatusTrial,
	})
	assert.ErrorIs(t, err, domain.ErrSlugAlreadyTaken)
	assert.False(t, created)
}

func TestTenantRepository_Insert_OtherUniqueViolationPassesThrough(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505", ConstraintName: "uq_tenants_realm_id_dedicated"}
	tx := &fakeTx{
		queryRowFn: func(context.Context, string, ...any) pgx.Row {
			return &fakeRow{err: pgErr}
		},
	}
	repo := NewTenantRepository(nil)
	_, _, err := repo.Insert(injectTx(context.Background(), tx), &domain.Tenant{
		Slug: "ok", Name: "T", Plan: domain.PlanStarter, Status: domain.StatusTrial,
	})
	require.Error(t, err)
	assert.NotErrorIs(t, err, domain.ErrSlugAlreadyTaken)
	var got *pgconn.PgError
	assert.ErrorAs(t, err, &got)
	assert.Equal(t, "uq_tenants_realm_id_dedicated", got.ConstraintName)
}
