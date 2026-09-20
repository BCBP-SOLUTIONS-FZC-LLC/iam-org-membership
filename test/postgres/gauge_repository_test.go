//go:build integration

// GaugeRepository (internal/adapter/outbound/postgres/gauge_repository.go)
// backs the four cmd/server metric-exporter goroutines (§11.2:
// iam_org_membership_tenant_ownerless, iam_org_membership_realm_sync_pending,
// iam_org_membership_seat_overage_active,
// iam_org_membership_pending_invitations_stale). It is cross-tenant and must run against
// the BYPASSRLS sysPool — these tests assert the real row-count SQL against
// the live schema, which a mocked-transaction unit test can't meaningfully
// verify (the whole point of each query is "does it match the actual
// column names/types on tenants/pending_invitations").
package postgres_test

import (
	"context"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGaugeRepo_CountOwnerlessTenants_CountsOnlyOwnerlessNonDeleted(t *testing.T) {
	t.Parallel()
	_, rawPool, sysPool := setupTestDB(t)
	ctx := context.Background()

	ownerless := seedTenant(t, ctx, rawPool, "gauge-ownerless-1")
	_, err := rawPool.Exec(ctx, `UPDATE tenants SET ownerless_since = now() WHERE id = $1`, ownerless)
	require.NoError(t, err)

	_ = seedTenant(t, ctx, rawPool, "gauge-ownerless-2") // not ownerless — must not be counted

	deletedOwnerless := seedTenant(t, ctx, rawPool, "gauge-ownerless-3")
	_, err = rawPool.Exec(ctx,
		`UPDATE tenants SET ownerless_since = now(), deleted_at = now() WHERE id = $1`, deletedOwnerless)
	require.NoError(t, err)

	repo := pgadapter.NewGaugeRepository(sysPool)
	n, err := repo.CountOwnerlessTenants(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "only the non-deleted ownerless tenant counts")
}

func TestGaugeRepo_CountRealmSyncPending_CountsOnlyPendingNonDeleted(t *testing.T) {
	t.Parallel()
	_, rawPool, sysPool := setupTestDB(t)
	ctx := context.Background()

	pending := seedTenant(t, ctx, rawPool, "gauge-sync-1")
	_, err := rawPool.Exec(ctx, `UPDATE tenants SET realm_sync_pending = true WHERE id = $1`, pending)
	require.NoError(t, err)

	_ = seedTenant(t, ctx, rawPool, "gauge-sync-2") // realm_sync_pending defaults false

	repo := pgadapter.NewGaugeRepository(sysPool)
	n, err := repo.CountRealmSyncPending(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
}

func TestGaugeRepo_CountSeatOverageActive_CountsOnlyOverageNonDeleted(t *testing.T) {
	t.Parallel()
	_, rawPool, sysPool := setupTestDB(t)
	ctx := context.Background()

	overage := seedTenant(t, ctx, rawPool, "gauge-overage-1")
	_, err := rawPool.Exec(ctx, `UPDATE tenants SET overage_since = now() WHERE id = $1`, overage)
	require.NoError(t, err)

	_ = seedTenant(t, ctx, rawPool, "gauge-overage-2")

	repo := pgadapter.NewGaugeRepository(sysPool)
	n, err := repo.CountSeatOverageActive(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
}

func TestGaugeRepo_CountPendingInvitationsStale_CountsOnlyExpiredPending(t *testing.T) {
	t.Parallel()
	_, rawPool, sysPool := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "gauge-invite-1")

	// trg_pending_invitations_expiry_guard (PI-1) only gates INSERT, not
	// UPDATE — insert with a future expires_at, then backdate it to
	// simulate a row that has since gone stale.
	staleID := uuid.New()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
		VALUES ($1, $2, 'stale@x.com', 'Stale Invitee', gen_random_uuid(), 'pending', now() + interval '1 day')`,
		staleID, tenantID)
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx,
		`UPDATE pending_invitations SET expires_at = now() - interval '1 day' WHERE id = $1`, staleID)
	require.NoError(t, err)

	freshID := uuid.New()
	_, err = rawPool.Exec(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
		VALUES ($1, $2, 'fresh@x.com', 'Fresh Invitee', gen_random_uuid(), 'pending', now() + interval '7 days')`,
		freshID, tenantID)
	require.NoError(t, err)

	revokedID := uuid.New()
	_, err = rawPool.Exec(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at)
		VALUES ($1, $2, 'revoked@x.com', 'Revoked Invitee', gen_random_uuid(), 'revoked', now() + interval '1 day')`,
		revokedID, tenantID)
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx,
		`UPDATE pending_invitations SET expires_at = now() - interval '1 day' WHERE id = $1`, revokedID)
	require.NoError(t, err)

	repo := pgadapter.NewGaugeRepository(sysPool)
	n, err := repo.CountPendingInvitationsStale(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "only the expired-but-still-pending invitation counts")
}

func TestGaugeRepo_AllCountsZeroOnEmptySchema(t *testing.T) {
	t.Parallel()
	_, _, sysPool := setupTestDB(t)
	ctx := context.Background()

	repo := pgadapter.NewGaugeRepository(sysPool)

	n1, err := repo.CountOwnerlessTenants(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n1)

	n2, err := repo.CountRealmSyncPending(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n2)

	n3, err := repo.CountSeatOverageActive(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n3)

	n4, err := repo.CountPendingInvitationsStale(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n4)

	counts, err := repo.RLSViolationCounts(ctx, 5*time.Minute)
	require.NoError(t, err)
	assert.Empty(t, counts)
}

// TestGaugeRepo_RLSViolationCounts_GroupsByTypeWithinWindow backs the
// iam_rls_violations_total exporter (§11.2) — verifies the real
// rls_violation_log column names/types (violation_type, occurred_at), the
// GROUP BY grouping, and that rows outside the trailing window are excluded.
// rls_violation_log has RLS disabled (recursion guard), so a direct INSERT
// via rawPool is the correct way to seed it — it is never written through
// the app pool in production either (only log_rls_violation() writes it).
func TestGaugeRepo_RLSViolationCounts_GroupsByTypeWithinWindow(t *testing.T) {
	t.Parallel()
	_, rawPool, sysPool := setupTestDB(t)
	ctx := context.Background()

	insert := func(violationType string, occurredAt time.Time) {
		_, err := rawPool.Exec(ctx, `
			INSERT INTO rls_violation_log (table_name, violation_type, occurred_at)
			VALUES ('tenant_memberships', $1, $2)`, violationType, occurredAt)
		require.NoError(t, err)
	}

	now := time.Now().UTC()
	insert("cross_tenant_access", now.Add(-1*time.Minute))
	insert("cross_tenant_access", now.Add(-2*time.Minute))
	insert("missing_or_invalid_guc", now.Add(-3*time.Minute))
	// Outside the 5-minute window — must not be counted.
	insert("cross_tenant_access", now.Add(-10*time.Minute))

	repo := pgadapter.NewGaugeRepository(sysPool)
	counts, err := repo.RLSViolationCounts(ctx, 5*time.Minute)
	require.NoError(t, err)
	assert.EqualValues(t, 2, counts["cross_tenant_access"], "only the 2 in-window rows count")
	assert.EqualValues(t, 1, counts["missing_or_invalid_guc"])
	assert.Len(t, counts, 2, "no other violation_type present")
}
