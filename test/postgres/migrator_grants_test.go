//go:build integration

// 000007_migrator_grants: the sysPool role, org_membership_migrator, can use
// the tables when another role ran the migrations. The rest of this suite
// binds the sysPool to the postgres superuser, which hides a missing grant.
package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/test/dbseed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

func TestMigratorGrants_SysPoolRoleNotOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, rawPool, _ := setupTestDB(t) // migrations ran as the postgres superuser

	superDSN := rawPool.Config().ConnString()
	migratorDSN := strings.Replace(superDSN, "postgres:testpassword@", "org_membership_migrator:"+migratorPassword+"@", 1)
	require.NotEqual(t, superDSN, migratorDSN)

	sysPool, err := newPoolWithRetry(ctx, pgcommon.Config{DSN: migratorDSN})
	require.NoError(t, err)
	t.Cleanup(sysPool.Close)

	slug := uniqSlug("grants")
	tenantID := seedTenant(t, ctx, rawPool, slug)

	// PUB-1 reads through the sysPool: permission denied answered 500.
	res, err := pgadapter.NewTenantRepository(sysPool).ResolveByIdentifier(ctx, slug)
	require.NoError(t, err)
	assert.True(t, res.Found)

	// Reconciler sweeps update and delete through the same role.
	raw, err := dbseed.New(ctx, migratorDSN)
	require.NoError(t, err)
	t.Cleanup(raw.Close)
	_, err = raw.Exec(ctx, `UPDATE tenants SET name = name WHERE id = $1`, tenantID)
	require.NoError(t, err, "UPDATE as the migrator")
	_, err = raw.Exec(ctx, `DELETE FROM processed_events WHERE false`)
	require.NoError(t, err, "DELETE as the migrator")
	_, err = raw.Exec(ctx, `SELECT count(*) FROM outbox_events`)
	require.NoError(t, err, "outbox tables are covered too")

	// The RLS audit trail is read-only to the sysPool: only the SECURITY
	// DEFINER log_rls_violation() writes it.
	_, err = raw.Exec(ctx, `SELECT count(*) FROM rls_violation_log`)
	require.NoError(t, err)
	_, err = raw.Exec(ctx, `DELETE FROM rls_violation_log`)
	require.ErrorContains(t, err, "permission denied", "the migrator cannot erase RLS violations")

	// A table the migrating role creates later inherits the grant.
	_, err = rawPool.Exec(ctx, `CREATE TABLE public.grants_probe (id int)`)
	require.NoError(t, err)
	_, err = raw.Exec(ctx, `INSERT INTO public.grants_probe VALUES (1)`)
	require.NoError(t, err, "default privileges cover tables created after 000007")
}
