//go:build integration

// GaugeRepository (internal/adapter/outbound/postgres/gauge_repository.go)
// backs the four cmd/server metric-exporter goroutines (§11.2:
// iam_tenant_ownerless, iam_realm_sync_pending, iam_seat_overage_active,
// iam_pending_invitations_stale). It is cross-tenant and must run against
// the BYPASSRLS sysPool — these tests assert the real row-count SQL against
// the live schema, which a mocked-transaction unit test can't meaningfully
// verify (the whole point of each query is "does it match the actual
// column names/types on tenants/pending_invitations").
package postgres_test

import (
	"context"
	"testing"

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
}
