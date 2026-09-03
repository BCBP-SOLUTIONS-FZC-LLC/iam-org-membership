//go:build integration

// Extended postgres-layer tests for I-16 (§16 OQ-9/RP-C3).
//
// Test case IDs: I16-GRACE-01..04, I16-STATUS-01..03/05/06,
// I16-GDPR-01, I16-RLS-01, I16-SCALE-01, I16-BIZ-01
//
// All tests run against a real Postgres container spun up by testcontainers.
// The SubscriptionLapseService is wired against the BYPASSRLS sysPool (same as
// production) so cross-tenant reads work as expected.
package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── seed helpers ─────────────────────────────────────────────────────────────

func lapseSetActive(t testing.TB, ctx context.Context, fx *testFixtures, id uuid.UUID) {
	t.Helper()
	_, err := fx.rawPool.Exec(ctx,
		`UPDATE tenants SET status = 'active', subscription_started_at = now() - interval '90 days',
		 cancelled_at = NULL WHERE id = $1`, id)
	require.NoError(t, err)
}

func lapseSetTrialExpired(t testing.TB, ctx context.Context, fx *testFixtures, id uuid.UUID) {
	t.Helper()
	_, err := fx.rawPool.Exec(ctx,
		`UPDATE tenants SET status = 'trial_expired', trial_ends_at = now() - interval '5 days'
		 WHERE id = $1`, id)
	require.NoError(t, err)
}

func lapseSetSuspended(t testing.TB, ctx context.Context, fx *testFixtures, id uuid.UUID) {
	t.Helper()
	_, err := fx.rawPool.Exec(ctx,
		`UPDATE tenants
		 SET status = 'suspended', suspension_source = 'billing_lapse',
		     subscription_started_at = now() - interval '90 days',
		     cancelled_at = now() - interval '40 days'
		 WHERE id = $1`, id)
	require.NoError(t, err)
}

func lapseSetOffboarded(t testing.TB, ctx context.Context, fx *testFixtures, id uuid.UUID) {
	t.Helper()
	_, err := fx.rawPool.Exec(ctx,
		`UPDATE tenants
		 SET status = 'offboarded',
		     subscription_started_at = now() - interval '90 days',
		     cancelled_at = now() - interval '60 days',
		     deleted_at = now()
		 WHERE id = $1`, id)
	require.NoError(t, err)
}

func lapseSetSoftDeleted(t testing.TB, ctx context.Context, fx *testFixtures, id uuid.UUID) {
	t.Helper()
	_, err := fx.rawPool.Exec(ctx,
		`UPDATE tenants SET deleted_at = now() WHERE id = $1`, id)
	require.NoError(t, err)
}

// ── I16-GRACE-01: exactly graceDays (30) cancelled → returned (query uses <=) ─

// Test Case ID: I16-GRACE-01
func TestSubscriptionLapse_ExactlyGraceDays_Returned(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	// cancelled exactly 30 days ago — <= boundary is inclusive
	exact := seedCancelledTenant(t, ctx, fx, "lapse-exact-30", 30)

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)

	ids := make(map[uuid.UUID]bool)
	for _, tn := range tenants {
		ids[tn.ID] = true
	}
	assert.True(t, ids[exact],
		"tenant cancelled exactly 30 days ago must appear — query uses <= not <")
}

// ── I16-GRACE-02: 29 days cancelled → NOT returned ───────────────────────────

// Test Case ID: I16-GRACE-02
func TestSubscriptionLapse_WithinGrace29Days_NotReturned(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	within := seedCancelledTenant(t, ctx, fx, "lapse-within-29", 29)

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)

	for _, tn := range tenants {
		assert.NotEqual(t, within, tn.ID,
			"tenant cancelled 29 days ago must NOT appear — still within 30-day grace")
	}
}

// ── I16-GRACE-03: 31 days cancelled → returned ───────────────────────────────

// Test Case ID: I16-GRACE-03
func TestSubscriptionLapse_JustPastGrace31Days_Returned(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	past := seedCancelledTenant(t, ctx, fx, "lapse-past-31", 31)

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)

	ids := make(map[uuid.UUID]bool)
	for _, tn := range tenants {
		ids[tn.ID] = true
	}
	assert.True(t, ids[past], "tenant cancelled 31 days ago must appear")
}

// ── I16-GRACE-04: mix 29d + 35d → only 35d returned ─────────────────────────

// Test Case ID: I16-GRACE-04
func TestSubscriptionLapse_MixedGracePeriods_OnlyPastGraceReturned(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	inside := seedCancelledTenant(t, ctx, fx, "lapse-mix-inside", 29)
	outside := seedCancelledTenant(t, ctx, fx, "lapse-mix-outside", 35)

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)

	ids := make(map[uuid.UUID]bool)
	for _, tn := range tenants {
		ids[tn.ID] = true
	}
	assert.False(t, ids[inside], "29-day tenant must be absent — inside grace")
	assert.True(t, ids[outside], "35-day tenant must appear — past grace")
}

// ── I16-STATUS-01: active tenant → excluded ──────────────────────────────────

// Test Case ID: I16-STATUS-01
func TestSubscriptionLapse_ActiveTenant_Excluded(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	// seedTenant creates a trial tenant; convert to active
	id := seedTenant(t, ctx, fx.rawPool, "lapse-status-active")
	lapseSetActive(t, ctx, fx, id)

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)
	for _, tn := range tenants {
		assert.NotEqual(t, id, tn.ID, "active tenant must never appear in lapse list")
	}
}

// ── I16-STATUS-02: trial tenant → excluded ───────────────────────────────────

// Test Case ID: I16-STATUS-02
func TestSubscriptionLapse_TrialTenant_Excluded(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	id := seedTenant(t, ctx, fx.rawPool, "lapse-status-trial")

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)
	for _, tn := range tenants {
		assert.NotEqual(t, id, tn.ID, "trial tenant must never appear in lapse list")
	}
}

// ── I16-STATUS-03: trial_expired tenant → excluded ───────────────────────────

// Test Case ID: I16-STATUS-03
func TestSubscriptionLapse_TrialExpiredTenant_Excluded(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	id := seedTenant(t, ctx, fx.rawPool, "lapse-status-trial-exp")
	lapseSetTrialExpired(t, ctx, fx, id)

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)
	for _, tn := range tenants {
		assert.NotEqual(t, id, tn.ID, "trial_expired tenant must not appear — separate RP-4 flow")
	}
}

// ── I16-STATUS-05: offboarded tenant → excluded ───────────────────────────────

// Test Case ID: I16-STATUS-05
func TestSubscriptionLapse_OffboardedTenant_Excluded(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	id := seedCancelledTenant(t, ctx, fx, "lapse-status-offboard", 35)
	lapseSetOffboarded(t, ctx, fx, id)

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)
	for _, tn := range tenants {
		assert.NotEqual(t, id, tn.ID, "offboarded tenant must not appear — past the lapse flow")
	}
}

// ── I16-STATUS-06: all statuses in DB → only cancelled+past-grace returned ───

// Test Case ID: I16-STATUS-06
func TestSubscriptionLapse_AllStatuses_OnlyCancelledPastGraceReturned(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	active := seedTenant(t, ctx, fx.rawPool, "lapse-all-active")
	lapseSetActive(t, ctx, fx, active)

	trial := seedTenant(t, ctx, fx.rawPool, "lapse-all-trial")

	trialExp := seedTenant(t, ctx, fx.rawPool, "lapse-all-trial-exp")
	lapseSetTrialExpired(t, ctx, fx, trialExp)

	suspended := seedCancelledTenant(t, ctx, fx, "lapse-all-suspended", 35)
	lapseSetSuspended(t, ctx, fx, suspended)

	offboarded := seedCancelledTenant(t, ctx, fx, "lapse-all-offboard", 35)
	lapseSetOffboarded(t, ctx, fx, offboarded)

	cancelled := seedCancelledTenant(t, ctx, fx, "lapse-all-cancelled", 35)

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)

	ids := make(map[uuid.UUID]bool)
	for _, tn := range tenants {
		ids[tn.ID] = true
	}
	assert.False(t, ids[active], "active must be excluded")
	assert.False(t, ids[trial], "trial must be excluded")
	assert.False(t, ids[trialExp], "trial_expired must be excluded")
	assert.False(t, ids[suspended], "suspended (already acted on) must be excluded")
	assert.False(t, ids[offboarded], "offboarded must be excluded")
	assert.True(t, ids[cancelled], "cancelled+past-grace must be the only one returned")
}

// ── I16-GDPR-01: soft-deleted tenant → excluded ──────────────────────────────

// Test Case ID: I16-GDPR-01
func TestSubscriptionLapse_SoftDeleted_Excluded(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	id := seedCancelledTenant(t, ctx, fx, "lapse-gdpr-soft-del", 35)
	lapseSetSoftDeleted(t, ctx, fx, id)

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)
	for _, tn := range tenants {
		assert.NotEqual(t, id, tn.ID,
			"soft-deleted (GDPR-wiped) tenant must never re-surface in lapse poll")
	}
}

// ── I16-RLS-01: multiple tenants from different realms → all returned + ordered

// Test Case ID: I16-RLS-01
func TestSubscriptionLapse_CrossTenant_AllReturnedOrderedByCancelledAtASC(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	// seed in reverse time order; result must be oldest first
	newest := seedCancelledTenant(t, ctx, fx, "lapse-rls-newest", 31)
	middle := seedCancelledTenant(t, ctx, fx, "lapse-rls-middle", 45)
	oldest := seedCancelledTenant(t, ctx, fx, "lapse-rls-oldest", 60)

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)

	ids := make(map[uuid.UUID]bool)
	for _, tn := range tenants {
		ids[tn.ID] = true
	}
	assert.True(t, ids[oldest], "oldest tenant (60 days) must be present")
	assert.True(t, ids[middle], "middle tenant (45 days) must be present")
	assert.True(t, ids[newest], "newest tenant (31 days) must be present")

	// verify ORDER BY cancelled_at ASC: oldest must appear before newest
	var oldestIdx, newestIdx int
	for i, tn := range tenants {
		switch tn.ID {
		case oldest:
			oldestIdx = i
		case newest:
			newestIdx = i
		}
	}
	assert.Less(t, oldestIdx, newestIdx,
		"results must be ordered by cancelled_at ASC (oldest first)")
}

// ── I16-SCALE-01: 50 tenants past grace → all 50 returned (no silent cap) ────

// Test Case ID: I16-SCALE-01
func TestSubscriptionLapse_50Tenants_AllReturnedNoTruncation(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	want := make(map[uuid.UUID]bool, 50)
	for i := 0; i < 50; i++ {
		id := seedCancelledTenant(t, ctx, fx, fmt.Sprintf("lapse-scale-%02d", i), 35)
		want[id] = true
	}

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)

	got := make(map[uuid.UUID]bool, len(tenants))
	for _, tn := range tenants {
		got[tn.ID] = true
	}
	for id := range want {
		assert.True(t, got[id], "all 50 seeded tenants must appear — no silent LIMIT/truncation")
	}
}

// ── I16-BIZ-01: cancelled then reactivated → NOT returned ────────────────────

// Test Case ID: I16-BIZ-01
func TestSubscriptionLapse_ReactivatedTenant_Excluded(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()

	// Start as cancelled past grace, then reactivate — simulates TenantReactivated
	id := seedCancelledTenant(t, ctx, fx, "lapse-biz-reactivated", 35)
	lapseSetActive(t, ctx, fx, id) // cancelled_at cleared, status=active

	tenants, err := fx.SubscriptionLapse.List(ctx)
	require.NoError(t, err)
	for _, tn := range tenants {
		assert.NotEqual(t, id, tn.ID,
			"reactivated tenant must not appear — pull model sees current state immediately")
	}
}
