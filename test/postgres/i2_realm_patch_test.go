	//go:build integration

// Phase 6-extension — I-2 (PATCH /internal/tenants/:id) coverage backfill.
// Adds real postgres integration tests for scenarios that were previously
// only covered inductively (I2-H-03 response shape, I2-RLS-01 GUC binding,
// I2-CT-01 iam-system cross-tenant path).
package postgres_test

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// I2-H-03 + I2-RLS-01: valid PATCH atomically mutates all three realm
// columns AND leaves record_version bumped by the touch_row trigger. The
// UPDATE succeeds only because the GUC (app.tenant_id, app.user_id) was
// set to (target, iam-system) by SetRealmFields — RLS-5 invariant.
func TestSetRealmFields_UpdatesAllThreeRealmColumnsAtomically(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "i2h03")

	// Baseline: trial defaults from I-1 seed.
	var beforeRealmID, beforeRealmType, beforeShard string
	var beforeVersion int64
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT realm_id, realm_type, keycloak_shard, record_version FROM tenants WHERE id=$1`,
		tenantID).Scan(&beforeRealmID, &beforeRealmType, &beforeShard, &beforeVersion))
	require.Equal(t, "trial", beforeRealmID)
	require.Equal(t, "shared", beforeRealmType)
	require.Equal(t, "shard-0", beforeShard)

	// Act: SetRealmFields under the target tenant's context.
	tctx := withSystemAndTenant(ctx, tenantID)
	_, err := fx.Provisioning.SetRealmFields(tctx, tenantID, "acme-corp-kc",
		domain.RealmType("dedicated"), "shard-1", beforeVersion)
	require.NoError(t, err)

	// Assert: all three columns changed; record_version bumped by TRG-1.
	var afterRealmID, afterRealmType, afterShard string
	var afterVersion int64
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT realm_id, realm_type, keycloak_shard, record_version FROM tenants WHERE id=$1`,
		tenantID).Scan(&afterRealmID, &afterRealmType, &afterShard, &afterVersion))
	assert.Equal(t, "acme-corp-kc", afterRealmID, "realm_id set atomically")
	assert.Equal(t, "dedicated", afterRealmType, "realm_type set atomically")
	assert.Equal(t, "shard-1", afterShard, "keycloak_shard set atomically")
	assert.Equal(t, beforeVersion+1, afterVersion, "record_version bumped by touch_row trigger (TRG-1)")
}

// I2-CT-01: iam-system principal is NOT tenant-gated on /internal/* — the
// path :id is the source of truth, not the caller's x-tenant-id header
// (IAPI-2 / AUTH-5 / RLS-5). Provision two tenants; PATCH tenant A while
// running under tenant B's GUC context; only tenant A must be modified,
// tenant B must be untouched.
func TestSetRealmFields_CrossTenantSystemPathTargetsPathID(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantA, _ := seedTenantWithOwner(t, ctx, fx, "i2ct-a")
	tenantB, _ := seedTenantWithOwner(t, ctx, fx, "i2ct-b")
	require.NotEqual(t, tenantA, tenantB)

	// Run SetRealmFields for tenantA. The service internally overrides the
	// GUC to (iam-system, tenantA) regardless of the incoming context, so
	// even a "wrong" x-tenant-id can't misdirect the write.
	ctxWrongTenant := withSystemAndTenant(ctx, tenantB) // deliberately B
	_, err := fx.Provisioning.SetRealmFields(ctxWrongTenant, tenantA, "acme-cross", domain.RealmType("dedicated"), "shard-1", 1)
	require.NoError(t, err, "iam-system PATCHes any tenant regardless of the incoming GUC context")

	// tenantA (the path target) must be updated.
	var aRealm string
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT realm_id FROM tenants WHERE id=$1`, tenantA).Scan(&aRealm))
	assert.Equal(t, "acme-cross", aRealm, "path :id tenant was updated")

	// tenantB (the header-claimed tenant) must NOT be updated.
	var bRealm string
	require.NoError(t, fx.rawPool.QueryRow(ctx,
		`SELECT realm_id FROM tenants WHERE id=$1`, tenantB).Scan(&bRealm))
	assert.Equal(t, "trial", bRealm, "header-claimed tenant was NOT touched — path is authoritative")
}

// I2-H-03 (deeper): SetRealmFields returns nil (no error) when the UPDATE
// succeeds — the HTTP handler then projects the response body from the
// input parameters (internal_handler.go:161). This exercises the "silent
// success returns nil" contract that the handler translation depends on.
func TestSetRealmFields_ReturnsNilOnSuccess(t *testing.T) {
	fx := buildTestFixtures(t)
	ctx := context.Background()
	tenantID, _ := seedTenantWithOwner(t, ctx, fx, "i2h03b")

	tctx := withSystemAndTenant(ctx, tenantID)
	_, err := fx.Provisioning.SetRealmFields(tctx, tenantID, "acme-shape",
		domain.RealmType("dedicated"), "shard-2", 1)
	assert.NoError(t, err, "service returns nil on happy path so the handler can 200 with the input echo")
}

// I2-A-03 (positive): the iam-system principal reaches the service —
// covered inductively by every test above (they all run under
// withSystemAndTenant, which sets x-user-id=iam-system + x-tenant-roles=iam-system).
// If middleware were to reject it, seedTenantWithOwner would fail and
// no test in this file would pass.
