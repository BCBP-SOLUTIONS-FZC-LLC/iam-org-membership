//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startPostgres spins up a fresh Postgres 17 container for this test and
// returns the superuser DSN + a cleanup func. Mirrors test/postgres
// setupTestDB minus the RLS-role dance — Phase 12 exercises the wire, not
// RLS, so a superuser pool is sufficient.
func startPostgres(t *testing.T, ctx context.Context) (dsn string, cleanup func()) {
	t.Helper()
	c, err := tcpostgres.Run(ctx,
		"postgres:17-alpine",
		tcpostgres.WithDatabase("org_membership"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("testpassword"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "start postgres")
	dsn, err = c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	return dsn, func() { _ = c.Terminate(ctx) }
}
