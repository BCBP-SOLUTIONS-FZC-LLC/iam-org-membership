//go:build integration

// Package postgres_test is the canonical Phase 1 test suite: spins up a
// fresh Postgres 17 container via testcontainers-go, applies every domain
// migration, and asserts the RLS/trigger/check invariants named in LLD §14.5.
//
// Requires Docker on the runner. Tag: integration.
package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	appRolePassword  = "apppassword-testonly"
	migratorPassword = "migratorpassword-testonly"
)

// setupTestDB spins up a Postgres 17 container, applies every migration
// (as superuser so DDL and CREATE ROLE succeed), creates the two runtime
// roles the LLD calls out (org_membership_app without BYPASSRLS,
// org_membership_migrator with BYPASSRLS), and returns:
//
//	appPool     — pgcommon.Pool bound as org_membership_app, RLS enforced,
//	              GUC-provider wired so `SET LOCAL app.tenant_id` fires on
//	              every checkout (mirrors production).
//	rawPool     — dbseed.Pool (pgcommon-backed superuser) used only to
//	              seed rows (bypasses RLS naturally).
//	sysPool     — pgcommon.Pool bound as postgres superuser, no GUCProvider,
//	              for tests that exercise jobs.Context.SysPool (a real
//	              *pgcommon.Pool in production, deliberately without RLS GUC
//	              injection so a BYPASSRLS-equivalent role sees every tenant).
func setupTestDB(t testing.TB) (*pgcommon.Pool, *dbseed.Pool, *pgcommon.Pool) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping postgres integration test in short mode")
	}
	ctx := context.Background()

	// Container creation + connection-string resolution is retried up to 3
	// times: under concurrent container churn (many test/postgres funcs now
	// run via t.Parallel()), testcontainers/Docker Desktop occasionally
	// loses the port-registration race — the wait strategy sees the "ready"
	// log line before the port mapping is queryable, surfacing as
	// `port "5432/tcp" not found` from ConnectionString. This is infra
	// timing noise, not a test defect, and self-heals on retry.
	const maxContainerAttempts = 3
	var pgContainer *tcpostgres.PostgresContainer
	var superDSN string
	var err error
	for attempt := 1; attempt <= maxContainerAttempts; attempt++ {
		pgContainer, err = tcpostgres.Run(ctx,
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
		if err == nil {
			superDSN, err = pgContainer.ConnectionString(ctx, "sslmode=disable")
			if err == nil {
				break
			}
			_ = pgContainer.Terminate(ctx)
		}
		t.Logf("setupTestDB: postgres testcontainer attempt %d/%d failed: %v", attempt, maxContainerAttempts, err)
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	require.NoError(t, err, "postgres testcontainer failed after %d attempts", maxContainerAttempts)
	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })

	rawPool, err := dbseed.New(ctx, superDSN)
	require.NoError(t, err)
	t.Cleanup(rawPool.Close)

	// Create the two runtime roles BEFORE running migrations. The domain
	// migration reasserts BYPASSRLS on the migrator and strips BYPASSRLS
	// from the app role if somehow acquired.
	_, err = rawPool.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE org_membership_app LOGIN PASSWORD '%s' NOBYPASSRLS`, appRolePassword))
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE org_membership_migrator LOGIN PASSWORD '%s' BYPASSRLS`, migratorPassword))
	require.NoError(t, err)

	// Apply platform-events outbox schema FIRST (creates outbox_events +
	// outbox_dead_letters, with a JSONB payload column) — the domain
	// migration ALTERs that same column to TEXT, so the outbox schema must
	// exist before domain migrations run (mirrors cmd/server/main.go's own
	// ordering comment). Phase 4 service-integration tests query
	// outbox_events directly to verify event emission.
	require.NoError(t, outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: superDSN}))

	// Apply migrations as superuser (needs CREATE EXTENSION, CREATE TYPE, etc).
	require.NoError(t, pgadapter.RunMigrations(ctx, superDSN))

	// Grant table + function privileges to org_membership_app so the
	// RLS-enforced pool can actually read/write. RLS still gates rows.
	grants := []string{
		`GRANT CONNECT ON DATABASE org_membership TO org_membership_app`,
		`GRANT USAGE ON SCHEMA public TO org_membership_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO org_membership_app`,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO org_membership_app`,
		`GRANT EXECUTE ON FUNCTION app_tenant_id()                    TO org_membership_app`,
		`GRANT EXECUTE ON FUNCTION log_rls_violation(text, uuid, text) TO org_membership_app`,
		`GRANT EXECUTE ON FUNCTION rls_check_tenant(uuid, text)       TO org_membership_app`,
	}
	for _, stmt := range grants {
		_, err = rawPool.Exec(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	// pgcommon pool as org_membership_app — RLS is fully enforced here.
	// GUCProvider wires the same transaction-local `SET LOCAL app.tenant_id`
	// binding used in production (RLS-6).
	appDSN := strings.Replace(superDSN, "postgres:testpassword@", "org_membership_app:"+appRolePassword+"@", 1)
	appPool, err := newPoolWithRetry(ctx, pgcommon.Config{
		DSN:           appDSN,
		PGBouncerMode: false, // testcontainer talks to Postgres directly
		GUCProvider:   pgcommon.GUCSetFromContext,
	})
	require.NoError(t, err, "app pool connection failed after retries")
	t.Cleanup(appPool.Close)

	sysPool, err := newPoolWithRetry(ctx, pgcommon.Config{DSN: superDSN})
	require.NoError(t, err, "sys pool connection failed after retries")
	t.Cleanup(sysPool.Close)

	return appPool, rawPool, sysPool
}

// newPoolWithRetry wraps pgcommon.NewPool with a bounded per-attempt timeout
// and retry, mirroring the container-creation retry above: under concurrent
// container/pool churn (many test/postgres funcs run via t.Parallel()), a
// resource-constrained CI runner can make one connection attempt stall.
// Without a timeout here, a stalled attempt hangs until the whole test
// binary's own -timeout kills every in-flight test, not just this one —
// this is exactly the failure mode a CI run surfaced (a goroutine stuck in
// pgxpool's createIdleResources for the full 300s package timeout).
// maxAttempts/attempt timeout widened from 3/30s: this branch's test/postgres
// suite runs ~3x main's container count (~227 vs ~80), so resource pressure
// on a hosted CI runner is proportionally higher and a stalled connection
// attempt needs more headroom to recover on retry rather than exhausting its
// budget and surfacing as a test failure.
func newPoolWithRetry(ctx context.Context, cfg pgcommon.Config) (*pgcommon.Pool, error) {
	const maxAttempts = 5
	var pool *pgcommon.Pool
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		pool, err = pgcommon.NewPool(attemptCtx, cfg)
		cancel()
		if err == nil {
			return pool, nil
		}
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	return nil, err
}

// withTenant returns a context carrying a pgcommon GUCSet so the pool's
// GUCProvider emits `SET LOCAL app.tenant_id = <uuid>` on every checkout.
func withTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	return pgcommon.WithGUCSet(ctx, g)
}

// seedTenant inserts a tenants row via the superuser pool. Bypasses RLS.
// Returns the tenant id.
func seedTenant(t testing.TB, ctx context.Context, rawPool *dbseed.Pool, slug string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO tenants (id, slug, name, plan, status, trial_ends_at)
		VALUES ($1, $2, $3, 'starter', 'trial', now() + interval '30 days')`,
		id, slug, slug)
	require.NoError(t, err)
	return id
}

// ─────────────────────────────────────────────────────────────────────────
// Case 1 (RLS-1): every tenant-scoped table has ENABLE + FORCE RLS.
// ─────────────────────────────────────────────────────────────────────────
func TestRLS_Case1_EveryTenantScopedTableEnabled(t *testing.T) {
	t.Parallel()
	_, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	// The 7 tenant-scoped tables remaining per LLD §4.3 — group_dept_role_mappings/
	// group_tenant_role_mappings/group_dept_mappings were dropped (ADR-0007
	// Wave 2, moved to Group Mapping Service); tender_acl_entries was dropped
	// (ADR-0007 Wave 3 Phase 7, moved to iam-tender-acl); delegations was
	// dropped (ADR-0008 v2 Option C, moved to iam-delegation).
	expected := []string{
		"tenants", "tenant_departments", "tenant_memberships", "tenant_roles",
		"dept_memberships", "dept_role_labels",
		"pending_invitations",
	}
	rows, err := rawPool.Query(ctx, `
		SELECT c.relname
		FROM pg_class c JOIN pg_namespace n ON c.relnamespace = n.oid
		WHERE n.nspname = 'public'
		  AND c.relkind = 'r'
		  AND c.relrowsecurity = true
		  AND c.relforcerowsecurity = true`)
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		got[name] = true
	}
	for _, e := range expected {
		assert.True(t, got[e], "expected %s to have ENABLE + FORCE ROW LEVEL SECURITY", e)
	}
	assert.Len(t, got, len(expected), "unexpected extra RLS tables: %v", got)
}

// ─────────────────────────────────────────────────────────────────────────
// Case 1b (MIG-5 / RLS-4): org_membership_app must never hold BYPASSRLS.
// The runtime app role is intended to be RLS-scoped; a stray BYPASSRLS
// grant would defeat every tenant_isolation policy silently. LLD line 1744
// promises "CI verifies that org_membership_app does not possess BYPASSRLS".
// ─────────────────────────────────────────────────────────────────────────
func TestRLS_Case1b_AppRoleHasNoBYPASSRLS(t *testing.T) {
	t.Parallel()
	_, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	var bypass bool
	err := rawPool.QueryRow(ctx, `
		SELECT rolbypassrls FROM pg_roles WHERE rolname = 'org_membership_app'`).Scan(&bypass)
	require.NoError(t, err, "org_membership_app role must exist in the test DB (see rls_test.go setup)")
	assert.False(t, bypass, "org_membership_app must NOT hold BYPASSRLS (RLS-4/MIG-5)")
}

// ─────────────────────────────────────────────────────────────────────────
// Case 2 (RLS-2): missing/malformed GUC → 0 rows, no writes.
// ─────────────────────────────────────────────────────────────────────────
func TestRLS_Case2_FailClosedOnMissingGUC(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	_ = seedTenant(t, ctx, rawPool, "acme")

	// No GUC in context → pool checkout binds no app.tenant_id → policy
	// returns false for every row → SELECT returns 0.
	var count int
	err := pgcommon.RunInTx(ctx, appPool, pgxTxOpts(), func(ctx context.Context, tx pgxTx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&count)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count, "RLS-2: missing GUC must return 0 rows")
}

// ─────────────────────────────────────────────────────────────────────────
// Case 3 (RLS-3): cross-tenant INSERT rejected by WITH CHECK.
// ─────────────────────────────────────────────────────────────────────────
func TestRLS_Case3_CrossTenantInsertRejectedByWithCheck(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "acme")
	tenantB := uuid.New()

	ctxA := withTenant(ctx, tenantA)
	// Under tenantA GUC, attempt to insert a tenant_membership row whose
	// tenant_id = tenantB → WITH CHECK compares row's tenant_id to
	// app.tenant_id → false → new row for relation violates policy.
	err := pgcommon.RunInTx(ctxA, appPool, pgxTxOpts(), func(ctx context.Context, tx pgxTx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO tenant_memberships (id, tenant_id, user_id)
			VALUES (gen_random_uuid(), $1, gen_random_uuid())`,
			tenantB)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "row-level security", "RLS-3: WITH CHECK must reject cross-tenant insert")
}

// ─────────────────────────────────────────────────────────────────────────
// Case 5 (RLS-6): no cross-tenant leak across a pooled connection.
// Canonical PgBouncer safety test — tenant A executes a tx, its
// connection returns to the pool, tenant B executes on (potentially) the
// same backend, and B must NOT see A's rows. Because pgcommon uses SET
// LOCAL app.tenant_id, the GUC auto-resets at COMMIT.
// ─────────────────────────────────────────────────────────────────────────
func TestRLS_Case5_NoCrossTenantLeakAcrossPool(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()

	tenantA := seedTenant(t, ctx, rawPool, "acme")
	tenantB := seedTenant(t, ctx, rawPool, "beta")

	// Force a single connection so B pins the same backend A used.
	// (pgcommon.NewPool default is >1 conns; single-conn stress focuses the test.)
	// We accomplish this by running the two ops serially — with a small pool
	// or a fresh pool this deterministically reuses the same backend.

	// Tenant A: read own tenant row (should see it).
	ctxA := withTenant(ctx, tenantA)
	var seenA string
	err := pgcommon.RunInTx(ctxA, appPool, pgxTxOpts(), func(ctx context.Context, tx pgxTx) error {
		return tx.QueryRow(ctx, `SELECT slug FROM tenants WHERE id = $1`, tenantA).Scan(&seenA)
	})
	require.NoError(t, err)
	assert.Equal(t, "acme", seenA)

	// Tenant B: read own tenant row + attempt to read A's row.
	ctxB := withTenant(ctx, tenantB)
	var seenB string
	err = pgcommon.RunInTx(ctxB, appPool, pgxTxOpts(), func(ctx context.Context, tx pgxTx) error {
		return tx.QueryRow(ctx, `SELECT slug FROM tenants WHERE id = $1`, tenantB).Scan(&seenB)
	})
	require.NoError(t, err)
	assert.Equal(t, "beta", seenB)

	// Tenant B tries to see tenant A's row → 0 rows (no leak).
	var count int
	err = pgcommon.RunInTx(ctxB, appPool, pgxTxOpts(), func(ctx context.Context, tx pgxTx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = $1`, tenantA).Scan(&count)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count, "RLS-6: tenant B must not see any of tenant A's rows on a pooled backend")
}

// ─────────────────────────────────────────────────────────────────────────
// T-1: slug UPDATE raises exception (trg_tenant_slug_immutable).
// ─────────────────────────────────────────────────────────────────────────
func TestT1_SlugIsImmutable(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "acme")

	ctxA := withTenant(ctx, tenantA)
	err := pgcommon.RunInTx(ctxA, appPool, pgxTxOpts(), func(ctx context.Context, tx pgxTx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET slug = 'renamed' WHERE id = $1`, tenantA)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tenant slug is immutable")
}

// ─────────────────────────────────────────────────────────────────────────
// TRG-3: no-op UPDATE does NOT bump record_version.
// ─────────────────────────────────────────────────────────────────────────
func TestTRG3_NoOpUpdateDoesNotBumpVersion(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "acme")

	ctxA := withTenant(ctx, tenantA)
	// status is already 'trial' after seed; setting it again is a no-op row.
	var before, after int64
	err := pgcommon.RunInTx(ctxA, appPool, pgxTxOpts(), func(ctx context.Context, tx pgxTx) error {
		if err := tx.QueryRow(ctx, `SELECT record_version FROM tenants WHERE id = $1`, tenantA).Scan(&before); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tenants SET status = 'trial' WHERE id = $1`, tenantA); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT record_version FROM tenants WHERE id = $1`, tenantA).Scan(&after)
	})
	require.NoError(t, err)
	assert.Equal(t, before, after, "TRG-3: no-op UPDATE must not bump record_version")
}

// ─────────────────────────────────────────────────────────────────────────
// TR-7 / chk_tr_no_member: INSERT with role_code='member' rejected.
// ─────────────────────────────────────────────────────────────────────────
func TestTR7_MemberRoleRejected(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "acme")

	ctxA := withTenant(ctx, tenantA)
	err := pgcommon.RunInTx(ctxA, appPool, pgxTxOpts(), func(ctx context.Context, tx pgxTx) error {
		userID := uuid.New()
		membershipID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenant_memberships (id, tenant_id, user_id) VALUES ($1, $2, $3)`,
			membershipID, tenantA, userID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO tenant_roles (tenant_id, user_id, tenant_membership_id, role_code, granted_by)
			 VALUES ($1, $2, $3, 'member', $2)`,
			tenantA, userID, membershipID)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chk_tr_no_member")
}

// ─────────────────────────────────────────────────────────────────────────
// Composite FK (§16 A15/A28, DM-4): dept_memberships INSERT with a
// (id, tenant_id, user_id) triple that does not match a real
// tenant_memberships row is rejected by fk_dm_tenant_membership.
// ─────────────────────────────────────────────────────────────────────────
func TestCompositeFK_DeptMembershipRejectsWrongUser(t *testing.T) {
	t.Parallel()
	appPool, rawPool, _ := setupTestDB(t)
	ctx := context.Background()
	tenantA := seedTenant(t, ctx, rawPool, "acme")

	ctxA := withTenant(ctx, tenantA)
	err := pgcommon.RunInTx(ctxA, appPool, pgxTxOpts(), func(ctx context.Context, tx pgxTx) error {
		userID := uuid.New()
		membershipID := uuid.New()
		wrongUserID := uuid.New()
		// department_id has no FK to a catalog table anymore (departments
		// was dropped — migration-runbook Phase 4, LLD §12 step 4), so any
		// UUID satisfies fk_dm_tenant_dept; this test is only exercising
		// the composite (id, tenant_id, user_id) FK below.
		deptID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenant_memberships (id, tenant_id, user_id) VALUES ($1, $2, $3)`,
			membershipID, tenantA, userID); err != nil {
			return err
		}
		// Activate a dept for the tenant so fk_dm_tenant_dept doesn't fail first.
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenant_departments (tenant_id, department_id) VALUES ($1, $2)`,
			tenantA, deptID); err != nil {
			return err
		}
		// Composite FK targets (id, tenant_id, user_id) — passing wrongUserID must fail.
		_, err := tx.Exec(ctx, `
			INSERT INTO dept_memberships (tenant_id, user_id, tenant_membership_id, department_id, role_level, granted_by)
			VALUES ($1, $2, $3, $4, 'preparator', $2)`,
			tenantA, wrongUserID, membershipID, deptID)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fk_dm_tenant_membership")
}
