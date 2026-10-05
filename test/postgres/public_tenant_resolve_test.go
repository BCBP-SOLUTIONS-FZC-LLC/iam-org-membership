//go:build integration

// PUB-1 (GET /api/v1/public/tenants/resolve, LLD §5.4/§8.11) against real
// Postgres: TenantRepository.ResolveByIdentifier on the BYPASSRLS sysPool.
package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
)

// uniqSlug keeps slugs unique across parallel tests sharing a container.
func uniqSlug(prefix string) string {
	return prefix + "-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
}

// seedDedicatedTenant is a converted (paid) tenant with its own realm.
func seedDedicatedTenant(t *testing.T, ctx context.Context, fx *testFixtures, slug string) uuid.UUID {
	t.Helper()
	id := seedPaidTenant(t, ctx, fx.rawPool, slug)
	_, err := fx.rawPool.Exec(ctx, `UPDATE tenants SET realm_type = 'dedicated', realm_id = $2 WHERE id = $1`, id, "tenant-"+slug)
	require.NoError(t, err)
	return id
}

// seedInvitation inserts an invitation expiring in 7 days (the insert-time
// guard requires a future expires_at; age it afterwards if needed).
func seedInvitation(t *testing.T, ctx context.Context, fx *testFixtures, tenantID uuid.UUID, email, status string) {
	t.Helper()
	acceptedAt := "NULL"
	if status == "accepted" {
		acceptedAt = "now()"
	}
	_, err := fx.rawPool.Exec(ctx, `
		INSERT INTO pending_invitations (id, tenant_id, email, full_name, invited_by, status, expires_at, accepted_at)
		VALUES (gen_random_uuid(), $1, $2, 'Invitee', $3, $4::invitation_status, now() + interval '7 days', `+acceptedAt+`)`,
		tenantID, email, uuid.New(), status)
	require.NoError(t, err)
}

func resolve(t *testing.T, fx *testFixtures, identifier string) domain.TenantResolution {
	t.Helper()
	res, err := fx.TenantResolution.Resolve(context.Background(), identifier)
	require.NoError(t, err)
	return res
}

func TestPUB1_ResolveByIdentifier_Slug_Dedicated(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	slug := uniqSlug("acme")
	seedDedicatedTenant(t, context.Background(), fx, slug)

	res := resolve(t, fx, slug)
	assert.True(t, res.Found)
	assert.Equal(t, "tenant-"+slug, res.Realm)
	assert.Equal(t, domain.RealmDedicated, res.RealmType)
	assert.Nil(t, res.TenantID, "a dedicated-realm tenant never carries tenant_id (PUBRES-4)")
}

func TestPUB1_ResolveByIdentifier_Slug_Shared(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	slug := uniqSlug("trialco")
	id := seedTenant(t, context.Background(), fx.rawPool, slug)

	res := resolve(t, fx, slug)
	assert.True(t, res.Found)
	assert.Equal(t, "trial", res.Realm)
	assert.Equal(t, domain.RealmShared, res.RealmType)
	require.NotNil(t, res.TenantID)
	assert.Equal(t, id, *res.TenantID)
}

func TestPUB1_ResolveByIdentifier_PendingInviteEmail(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	slug := uniqSlug("invco")
	id := seedDedicatedTenant(t, ctx, fx, slug)
	email := uniqSlug("new.hire") + "@invco.example"
	seedInvitation(t, ctx, fx, id, email, "pending")

	res := resolve(t, fx, email)
	assert.True(t, res.Found)
	assert.Equal(t, "tenant-"+slug, res.Realm)

	upper := resolve(t, fx, strings.ToUpper(email))
	assert.True(t, upper.Found, "email matching is case-insensitive (citext)")
}

// Pins the scope decision (LLD §16 OQ-11, option A): O&M stores no email
// for an active member, so an active member's email is never resolvable.
// If option (B) (an iam-user-profile email lookup) is adopted, this flips.
func TestPUB1_ResolveByIdentifier_ActiveMemberEmail_NotFound(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	id, _ := seedTenantWithOwner(t, ctx, fx, uniqSlug("activeco"))
	email := uniqSlug("member") + "@activeco.example"
	// The invite that once existed has been accepted; the member is active.
	seedInvitation(t, ctx, fx, id, email, "accepted")

	assert.False(t, resolve(t, fx, email).Found)
}

func TestPUB1_ResolveByIdentifier_ExpiredInvite_NotFound(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	id := seedTenant(t, ctx, fx.rawPool, uniqSlug("expco"))
	email := uniqSlug("late") + "@expco.example"
	// Still 'pending' (the expiry cron hasn't swept it) but past expires_at.
	// The insert-time guard requires a future expires_at, so age it after.
	seedInvitation(t, ctx, fx, id, email, "pending")
	_, err := fx.rawPool.Exec(ctx, `UPDATE pending_invitations SET expires_at = now() - interval '1 hour' WHERE email = $1`, email)
	require.NoError(t, err)

	assert.False(t, resolve(t, fx, email).Found)
}

func TestPUB1_ResolveByIdentifier_DeletedTenant_NotFound(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	slug := uniqSlug("goneco")
	id := seedTenant(t, ctx, fx.rawPool, slug)
	_, err := fx.rawPool.Exec(ctx, `UPDATE tenants SET deleted_at = now() WHERE id = $1`, id)
	require.NoError(t, err)

	assert.False(t, resolve(t, fx, slug).Found)
}

// A slug match wins over an email match for the same identifier string.
func TestPUB1_ResolveByIdentifier_SlugBeatsInviteEmail(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	ctx := context.Background()
	shared := uniqSlug("clash")
	slugTenant := seedTenant(t, ctx, fx.rawPool, shared)
	other := seedDedicatedTenant(t, ctx, fx, uniqSlug("otherco"))
	seedInvitation(t, ctx, fx, other, shared+"@x.example", "pending")

	res := resolve(t, fx, shared)
	require.True(t, res.Found)
	require.NotNil(t, res.TenantID)
	assert.Equal(t, slugTenant, *res.TenantID)
}

func TestPUB1_ResolveByIdentifier_NoMatch(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	res := resolve(t, fx, "nobody-"+uuid.NewString()+"@example.com")
	assert.Equal(t, domain.TenantResolution{}, res)
}

// Why sysPool is mandatory: on the RLS-scoped app pool, with no
// app.tenant_id GUC bound (PUB-1 never binds one), RLS fails closed and a
// real tenant comes back Found=false. Mirrors rls_test.go Case 5's proof
// that the GUC-less path returns nothing.
func TestPUB1_ResolveByIdentifier_UsesBypassRLSPool(t *testing.T) {
	t.Parallel()
	fx := buildTestFixtures(t)
	slug := uniqSlug("rlsco")
	seedTenant(t, context.Background(), fx.rawPool, slug)

	require.True(t, resolve(t, fx, slug).Found, "precondition: sysPool resolves it")

	wrong := pgadapter.NewTenantRepository(fx.appPool)
	res, err := wrong.ResolveByIdentifier(context.Background(), slug)
	require.NoError(t, err, "RLS fails closed silently: no error, just no rows")
	assert.False(t, res.Found, "the RLS-scoped app pool must not see the tenant with no GUC bound")
}
