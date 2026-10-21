//go:build integration

// Signup provisioning backstop (TenantIdentityProvisioned, BFF-2 option B)
// against real Postgres: the real I-1 flow, RLS, processed_events and the
// missing-tenant retry rule.
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/inbound/consumer"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
)

func backstopTestConsumer(fx *testFixtures) *consumer.MembershipEventConsumer {
	return consumer.NewMembershipEventConsumer(
		pgadapter.NewInboxRepository(fx.appPool, &captureOutbox{}),
		pgadapter.NewTenantRepository(fx.appPool), fx.CatalogPlans, nil, 5*time.Minute, nil,
	).WithTenantProvisioner(fx.Provisioning)
}

func tipPayload(tenantID, owner uuid.UUID, slug string) map[string]any {
	return map[string]any{
		"tenant_id": tenantID, "owner_user_id": owner, "slug": slug, "name": "Backstop " + slug,
		"plan": "pro", "signup_type": "trial", "realm": "trial", "realm_type": "shared",
	}
}

func processedCount(t *testing.T, fx *testFixtures, eventID string) int {
	t.Helper()
	var n int
	require.NoError(t, fx.rawPool.QueryRow(context.Background(),
		`SELECT count(*) FROM processed_events WHERE event_id = $1::uuid`, eventID).Scan(&n))
	return n
}

// The signup's own I-1 never happened: the event creates the tenant exactly
// as I-1 would — owner membership, tenant_owner grant, 5 system departments.
func TestBackstop_CreatesMissingTenantViaI1(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, owner := uuid.New(), uuid.New()
	c := backstopTestConsumer(fx)

	env := mkEnvelope(t, "TenantIdentityProvisioned", tenantID, time.Now().UTC(), tipPayload(tenantID, owner, "bs-create"))
	require.NoError(t, c.Handle(ctx, env))

	var slug, name, plan, status string
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT slug, name, plan, status FROM tenants WHERE id = $1`, tenantID).
		Scan(&slug, &name, &plan, &status))
	assert.Equal(t, "bs-create", slug)
	assert.Equal(t, "Backstop bs-create", name)
	assert.Equal(t, "pro", plan)
	assert.Equal(t, "trial", status)

	var role string
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT tr.role_code FROM tenant_roles tr JOIN tenant_memberships tm ON tm.id = tr.tenant_membership_id
		 WHERE tm.tenant_id = $1 AND tm.user_id = $2 AND tm.status = 'active'`, tenantID, owner).Scan(&role))
	assert.Equal(t, "tenant_owner", role, "owner_user_id becomes the tenant_owner")

	var depts int
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT count(*) FROM tenant_departments WHERE tenant_id = $1`, tenantID).Scan(&depts))
	assert.Equal(t, 5, depts, "the 5 default system departments")
	assert.Equal(t, 1, processedCount(t, fx, env.ID))
}

// The BFF's I-1 succeeded first: the event changes nothing.
func TestBackstop_TenantAlreadyCreatedByBFF_NoChange(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, owner := uuid.New(), uuid.New()
	_, created, err := fx.Provisioning.TrialSignup(ctx, service.TrialSignupInput{
		TenantID: tenantID, Slug: "bs-bff-first", Name: "From BFF", Plan: domain.PlanStarter, OwnerUserID: owner,
	})
	require.NoError(t, err)
	require.True(t, created)

	env := mkEnvelope(t, "TenantIdentityProvisioned", tenantID, time.Now().UTC(), tipPayload(tenantID, owner, "bs-bff-first"))
	require.NoError(t, backstopTestConsumer(fx).Handle(ctx, env))

	var name, plan string
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT name, plan FROM tenants WHERE id = $1`, tenantID).Scan(&name, &plan))
	assert.Equal(t, "From BFF", name, "first writer wins")
	assert.Equal(t, "starter", plan)
	var members int
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT count(*) FROM tenant_memberships WHERE tenant_id = $1`, tenantID).Scan(&members))
	assert.Equal(t, 1, members, "no duplicate owner membership")
}

// Unordered delivery: RP's TenantRealmReady arrives before the tenant exists.
// It is retried (not recorded), the backstop creates the tenant, and the
// redelivered TenantRealmReady then applies.
func TestBackstop_EarlyLifecycleEventRetriedThenApplied(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, owner := uuid.New(), uuid.New()
	c := backstopTestConsumer(fx)

	realmReady := mkEnvelope(t, "TenantRealmReady", tenantID, time.Now().UTC(),
		map[string]any{"tenant_id": tenantID, "realm": "tenant-bs-early", "keycloak_shard": "shard-1"})
	err := c.Handle(ctx, realmReady)
	require.Error(t, err, "tenant not created yet — must be retried, not dropped")
	assert.Equal(t, 0, processedCount(t, fx, realmReady.ID), "not recorded, so the redelivery is processed")

	tip := mkEnvelope(t, "TenantIdentityProvisioned", tenantID, time.Now().UTC().Add(-time.Second), tipPayload(tenantID, owner, "bs-early"))
	require.NoError(t, c.Handle(ctx, tip))

	require.NoError(t, c.Handle(ctx, realmReady), "redelivery after the tenant exists")
	var realmID, realmType string
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT realm_id, realm_type FROM tenants WHERE id = $1`, tenantID).Scan(&realmID, &realmType))
	assert.Equal(t, "tenant-bs-early", realmID)
	assert.Equal(t, "dedicated", realmType)
	assert.Equal(t, 1, processedCount(t, fx, realmReady.ID))
}

// A slug held by another tenant can never succeed: rejected to the DLQ,
// nothing created, nothing recorded.
func TestBackstop_SlugTakenByAnotherTenant_Rejected(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	_, _, err := fx.Provisioning.TrialSignup(ctx, service.TrialSignupInput{
		TenantID: uuid.New(), Slug: "bs-taken", Name: "Other", Plan: domain.PlanPro, OwnerUserID: uuid.New(),
	})
	require.NoError(t, err)

	tenantID := uuid.New()
	env := mkEnvelope(t, "TenantIdentityProvisioned", tenantID, time.Now().UTC(), tipPayload(tenantID, uuid.New(), "bs-taken"))
	err = backstopTestConsumer(fx).Handle(ctx, env)
	require.ErrorIs(t, err, consumer.ErrProvisioningRejected)

	var n int
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = $1`, tenantID).Scan(&n))
	assert.Zero(t, n)
	assert.Equal(t, 0, processedCount(t, fx, env.ID))
}

// An offboarded (soft-deleted) tenant is never recreated, and lifecycle
// events for it are still dropped rather than retried.
func TestBackstop_OffboardedTenant_NotRecreated(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, owner := uuid.New(), uuid.New()
	_, _, err := fx.Provisioning.TrialSignup(ctx, service.TrialSignupInput{
		TenantID: tenantID, Slug: "bs-offboarded", Name: "Gone", Plan: domain.PlanPro, OwnerUserID: owner,
	})
	require.NoError(t, err)
	_, err = fx.rawPool.Exec(ctx, `UPDATE tenants SET status = 'offboarded', subscription_started_at = now(), cancelled_at = now(), deleted_at = now() WHERE id = $1`, tenantID)
	require.NoError(t, err)
	c := backstopTestConsumer(fx)

	tip := mkEnvelope(t, "TenantIdentityProvisioned", tenantID, time.Now().UTC(), tipPayload(tenantID, owner, "bs-offboarded"))
	require.NoError(t, c.Handle(ctx, tip))
	var deleted bool
	require.NoError(t, fx.rawPool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM tenants WHERE id = $1`, tenantID).Scan(&deleted))
	assert.True(t, deleted, "still offboarded")

	late := mkEnvelope(t, "TrialExpired", tenantID, time.Now().UTC(), map[string]any{"tenant_id": tenantID})
	require.NoError(t, c.Handle(ctx, late), "an event for an offboarded tenant is dropped, not retried")
	assert.Equal(t, 1, processedCount(t, fx, late.ID))
}
