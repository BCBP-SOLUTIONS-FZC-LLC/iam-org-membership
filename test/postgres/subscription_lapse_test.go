//go:build integration

// I-16 (§16 RP-C3) — RP's subscription-lapse sweep polls this to learn
// which tenants have crossed the cancellation grace period, since it has
// no other way to learn cancelled_at. Verifies the cross-tenant,
// BYPASSRLS-bound query against real Postgres: past-grace tenants are
// returned, within-grace and non-cancelled tenants are excluded.
package postgres_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedCancelledTenant(t testing.TB, ctx context.Context, fx *testFixtures, slug string, cancelledDaysAgo int) uuid.UUID {
	t.Helper()
	tenantID := seedTenant(t, ctx, fx.rawPool, slug)
	_, err := fx.rawPool.Exec(ctx, `
		UPDATE tenants
		SET status = 'cancelled',
		    subscription_started_at = now() - interval '90 days',
		    cancelled_at = now() - ($1::int * interval '1 day')
		WHERE id = $2`, cancelledDaysAgo, tenantID)
	require.NoError(t, err)
	return tenantID
}

func TestSubscriptionLapse_ReturnsOnlyPastGraceCancelledTenants(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	pastGrace := seedCancelledTenant(t, ctx, fx, "lapse-past-grace", 31)
	withinGrace := seedCancelledTenant(t, ctx, fx, "lapse-within-grace", 5)
	activeTenant := seedTenant(t, ctx, fx.rawPool, "lapse-active")

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)

	ids := make(map[uuid.UUID]bool, len(tenants))
	for _, tn := range tenants {
		ids[tn.ID] = true
		assert.Equal(t, domain.StatusCancelled, tn.Status)
		require.NotNil(t, tn.CancelledAt)
	}
	assert.True(t, ids[pastGrace], "tenant cancelled 31 days ago (grace=30) must be returned")
	assert.False(t, ids[withinGrace], "tenant cancelled 5 days ago must NOT be returned — still in grace")
	assert.False(t, ids[activeTenant], "a trial/active tenant must never be returned")
}

func TestSubscriptionLapse_SelfIdempotent_SuspendedTenantDropsOff(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	tenantID := seedCancelledTenant(t, ctx, fx, "lapse-suspend-me", 45)

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)
	found := false
	for _, tn := range tenants {
		if tn.ID == tenantID {
			found = true
		}
	}
	assert.True(t, found, "must appear before RP acts on it")

	// Simulate RP-C3 emitting TenantSuspended(source=billing_lapse): O&M
	// flips status to 'suspended'. The next poll must exclude it —
	// self-idempotent, no ack/cursor needed.
	_, err = fx.rawPool.Exec(ctx,
		`UPDATE tenants SET status = 'suspended', suspension_source = 'billing_lapse' WHERE id = $1`, tenantID)
	require.NoError(t, err)

	tenants, err = fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)
	for _, tn := range tenants {
		assert.NotEqual(t, tenantID, tn.ID, "suspended tenant must drop off the next poll")
	}
}
