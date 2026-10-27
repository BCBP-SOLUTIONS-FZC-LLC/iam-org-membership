//go:build integration

// EVT-18 (CD-50) against real Postgres: an event for a tenant with no row
// yet is parked in pending_tenant_events and acknowledged, then replayed in
// order by I-1 (both the BFF path and the backstop) or by the
// parked-events-replay sweep.
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/cmd/reconciler/jobs"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
)

// parkingTestConsumer is the production wiring: the consumer parks, and
// fx.Provisioning (I-1) takes the provisioning lock and replays. With
// wireI1=false, I-1 creates tenants without replaying, which leaves parked
// events for the sweep.
func parkingTestConsumer(fx *testFixtures, wireI1 bool) *consumer.MembershipEventConsumer {
	store := pgadapter.NewParkedTenantEventRepository(fx.appPool)
	c := consumer.NewMembershipEventConsumer(
		pgadapter.NewInboxRepository(fx.appPool, &captureOutbox{}),
		pgadapter.NewTenantRepository(fx.appPool), fx.CatalogPlans, nil, 5*time.Minute, nil,
	).WithTenantProvisioner(fx.Provisioning).
		WithParkedEvents(store, pgadapter.NewTxRunner(fx.appPool, &captureOutbox{}))
	if wireI1 {
		fx.Provisioning.WithParkedEvents(store, c)
	}
	return c
}

func parkedCount(t *testing.T, fx *testFixtures, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, fx.rawPool.QueryRow(context.Background(),
		`SELECT count(*) FROM pending_tenant_events WHERE tenant_id = $1`, tenantID).Scan(&n))
	return n
}

func tenantState(t *testing.T, fx *testFixtures, tenantID uuid.UUID) (status, plan, realmID, realmType string) {
	t.Helper()
	require.NoError(t, fx.rawPool.QueryRow(context.Background(),
		`SELECT status, plan, realm_id, realm_type FROM tenants WHERE id = $1`, tenantID).
		Scan(&status, &plan, &realmID, &realmType))
	return
}

// CD-50's exact case: RP-2's TenantRealmReady and TenantConverted reach Core
// before the Signup BFF's I-1. Both are parked and acknowledged (no retry,
// no DLQ); I-1 then creates the tenant and replays them, so it ends active,
// on the paid plan, in its dedicated realm.
func TestParked_DirectPaidEventsBeforeI1_ReplayedByI1(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, owner := uuid.New(), uuid.New()
	c := parkingTestConsumer(fx, true)
	t0 := time.Now().UTC().Add(-time.Minute)

	realmReady := mkEnvelope(t, "TenantRealmReady", tenantID, t0,
		map[string]any{"tenant_id": tenantID, "realm": "tenant-cd50", "keycloak_shard": "shard-1"})
	converted := mkEnvelope(t, "TenantConverted", tenantID, t0.Add(time.Second),
		map[string]any{"tenant_id": tenantID, "plan": "enterprise", "direct_signup": true})
	// Delivered out of order, both before the tenant exists.
	require.NoError(t, c.Handle(ctx, converted), "parked, not retried")
	require.NoError(t, c.Handle(ctx, realmReady), "parked, not retried")
	assert.Equal(t, 1, processedCount(t, fx, converted.ID), "acknowledged")
	assert.Equal(t, 1, processedCount(t, fx, realmReady.ID), "acknowledged")
	assert.Equal(t, 2, parkedCount(t, fx, tenantID))

	// The BFF's I-1.
	_, created, err := fx.Provisioning.TrialSignup(ctx, service.TrialSignupInput{
		TenantID: tenantID, Slug: "cd50-direct", Name: "CD-50 Direct", Plan: domain.PlanEnterprise, OwnerUserID: owner,
	})
	require.NoError(t, err)
	require.True(t, created)

	status, plan, realmID, realmType := tenantState(t, fx, tenantID)
	assert.Equal(t, "active", status, "TenantConverted replayed")
	assert.Equal(t, "enterprise", plan)
	assert.Equal(t, "tenant-cd50", realmID, "TenantRealmReady replayed")
	assert.Equal(t, "dedicated", realmType)
	assert.Zero(t, parkedCount(t, fx, tenantID), "both claimed")

	// A redelivery of an already-parked-and-replayed event is a duplicate.
	require.NoError(t, c.Handle(ctx, converted))
	assert.Zero(t, parkedCount(t, fx, tenantID))
}

// The BFF's I-1 never ran: the TenantIdentityProvisioned backstop creates
// the tenant through the same I-1 and replays the parked conversion.
func TestParked_BackstopI1AlsoReplays(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, owner := uuid.New(), uuid.New()
	c := parkingTestConsumer(fx, true)

	converted := mkEnvelope(t, "TenantConverted", tenantID, time.Now().UTC().Add(-2*time.Second),
		map[string]any{"tenant_id": tenantID, "plan": "pro", "direct_signup": true})
	require.NoError(t, c.Handle(ctx, converted))
	assert.Equal(t, 1, parkedCount(t, fx, tenantID))

	tip := mkEnvelope(t, "TenantIdentityProvisioned", tenantID, time.Now().UTC().Add(-time.Second),
		tipPayload(tenantID, owner, "cd50-backstop"))
	require.NoError(t, c.Handle(ctx, tip))

	status, plan, _, _ := tenantState(t, fx, tenantID)
	assert.Equal(t, "active", status)
	assert.Equal(t, "pro", plan)
	assert.Zero(t, parkedCount(t, fx, tenantID))
}

// I-1 committed but never replayed (here: I-1 not wired, as after a crash
// between commit and replay). The sweep finds the tenant and replays.
func TestParked_SweepReplaysWhatI1Missed(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, owner := uuid.New(), uuid.New()
	c := parkingTestConsumer(fx, false)

	converted := mkEnvelope(t, "TenantConverted", tenantID, time.Now().UTC().Add(-time.Second),
		map[string]any{"tenant_id": tenantID, "plan": "pro", "direct_signup": true})
	require.NoError(t, c.Handle(ctx, converted))

	orphanTenant := uuid.New() // parked, but this tenant never gets a row
	require.NoError(t, c.Handle(ctx, mkEnvelope(t, "TenantConverted", orphanTenant, time.Now().UTC(),
		map[string]any{"tenant_id": orphanTenant, "plan": "pro"})))

	_, _, err := fx.Provisioning.TrialSignup(ctx, service.TrialSignupInput{
		TenantID: tenantID, Slug: "cd50-sweep", Name: "CD-50 Sweep", Plan: domain.PlanPro, OwnerUserID: owner,
	})
	require.NoError(t, err)
	status, _, _, _ := tenantState(t, fx, tenantID)
	require.Equal(t, "trial", status, "no replay yet")

	res, err := jobs.ParkedEventsReplay(ctx, &jobs.Context{
		Reconciler:     pgadapter.NewReconcilerStore(fx.sysPool),
		ParkedReplayer: c,
		Logger:         port.NewSlogStyleLogger(nil),
		BatchLimit:     100,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Succeeded, "only the tenant whose row exists")

	status, _, _, _ = tenantState(t, fx, tenantID)
	assert.Equal(t, "active", status)
	assert.Zero(t, parkedCount(t, fx, tenantID))
	assert.Equal(t, 1, parkedCount(t, fx, orphanTenant), "still waiting for its tenant")

	var stale int64
	_, err = fx.rawPool.Exec(ctx, `UPDATE pending_tenant_events SET parked_at = now() - interval '11 minutes' WHERE tenant_id = $1`, orphanTenant)
	require.NoError(t, err)
	stale, err = pgadapter.NewGaugeRepository(fx.sysPool).CountParkedTenantEventsStale(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stale, int64(1), "the orphan shows on the stale gauge")
}

// pending_tenant_events is tenant-isolated like every other domain table.
func TestParked_RLS_OtherTenantCannotSeeParkedEvents(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID := uuid.New()
	c := parkingTestConsumer(fx, false)
	require.NoError(t, c.Handle(ctx, mkEnvelope(t, "TenantPlanChanged", tenantID, time.Now().UTC(),
		map[string]any{"tenant_id": tenantID, "plan": "pro"})))

	store := pgadapter.NewParkedTenantEventRepository(fx.appPool)
	other, err := store.ListForTenant(withTenant(ctx, uuid.New()), tenantID)
	require.NoError(t, err)
	assert.Empty(t, other, "RLS hides another tenant's parked events")
	own, err := store.ListForTenant(withTenant(ctx, tenantID), tenantID)
	require.NoError(t, err)
	assert.Len(t, own, 1)
}
