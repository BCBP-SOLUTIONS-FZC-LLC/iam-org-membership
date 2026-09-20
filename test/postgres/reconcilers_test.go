//go:build integration

// Phase 18 · 0%-units sweep — the four reconciler jobs that had no
// direct coverage: outbox_prune, trial_cleanup, realm_config_sync,
// invitation_kc_cleanup. Each test seeds a pre-condition state, invokes
// the job with a jobs.Context wired to a fake outbound client where
// relevant, and asserts on the observable side-effects (row deletes,
// marker clears, RP calls made).
//
// The four pre-existing job tests (invitation_expiry / delegation_expiry /
// seat_overage / processed_events_prune) are covered by Phase 5/reconciler_
// convergence_test.go and Phase 15.
package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/test/dbseed"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/cmd/reconciler/jobs"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
)

// ── shared RP fake for reconciler tests ─────────────────────────────────────

type recFakeRP struct {
	deleteUserErr   error
	deleteUserCalls []uuid.UUID
	patchErr        error
	patchCalls      []port.RealmConfigPatch
}

func (r *recFakeRP) CreateInvitedUser(_ context.Context, _ port.CreateInvitedUserRequest) (*port.CreateInvitedUserResponse, error) {
	return &port.CreateInvitedUserResponse{KeycloakUserID: uuid.New()}, nil
}

func (r *recFakeRP) DeleteUser(_ context.Context, _, kcUserID uuid.UUID) error {
	r.deleteUserCalls = append(r.deleteUserCalls, kcUserID)
	return r.deleteUserErr
}

func (r *recFakeRP) PatchRealmConfig(_ context.Context, _ uuid.UUID, p port.RealmConfigPatch) error {
	r.patchCalls = append(r.patchCalls, p)
	return r.patchErr
}

func (r *recFakeRP) RevokeUserSessions(_ context.Context, _, _ uuid.UUID) error { return nil }
func (r *recFakeRP) ResetMFA(_ context.Context, _, _ uuid.UUID) error           { return nil }

// ── P18-REC-OUTBOX-001 ──────────────────────────────────────────────────────

// TestREC_OUTBOX_001_PruneDropsOnlyRowsPastRetention — seed 3
// old-published + 3 recent-published + 2 unpublished; assert only the 3
// old rows disappear after OutboxPrune with retention_days=8.
func TestREC_OUTBOX_001_PruneDropsOnlyRowsPastRetention(t *testing.T) {
	t.Parallel()
	_, rawPool, sysPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := seedTenant(t, ctx, rawPool, "outbox-prune-018")

	// Old published — outside 8-day window.
	oldIDs := insertOutboxRows(t, ctx, rawPool, tenantID, 3, "-10 days", true)
	// Recent published — inside window.
	recentIDs := insertOutboxRows(t, ctx, rawPool, tenantID, 3, "-1 day", true)
	// Unpublished — never touched by prune.
	unpubIDs := insertOutboxRows(t, ctx, rawPool, tenantID, 2, "-30 days", false)

	outboxRunner, err := outbox.NewRunner(outbox.Config{
		Pool:      sysPool,
		Publisher: eventbusadapter.NoopPublisher{},
	})
	require.NoError(t, err)

	res, err := jobs.OutboxPrune(ctx, &jobs.Context{
		Reconciler:          pgadapter.NewReconcilerStore(sysPool),
		OutboxRunner:        outboxRunner,
		OutboxRetentionDays: 8,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, res.Succeeded, "must delete exactly the 3 old published rows")

	// Old rows gone.
	for _, id := range oldIDs {
		assert.False(t, outboxRowExists(t, ctx, rawPool, id),
			"old published row %s must be gone", id)
	}
	// Recent + unpublished remain.
	for _, id := range append(recentIDs, unpubIDs...) {
		assert.True(t, outboxRowExists(t, ctx, rawPool, id),
			"recent or unpublished row %s must survive", id)
	}
}

// TestREC_OUTBOX_002_EmptyTableIsNoOp — prune against a clean
// outbox_events table returns Attempted=Succeeded=0, no error.
func TestREC_OUTBOX_002_EmptyTableIsNoOp(t *testing.T) {
	t.Parallel()
	_, _, sysPool := setupTestDB(t)
	ctx := context.Background()
	outboxRunner, err := outbox.NewRunner(outbox.Config{
		Pool:      sysPool,
		Publisher: eventbusadapter.NoopPublisher{},
	})
	require.NoError(t, err)

	res, err := jobs.OutboxPrune(ctx, &jobs.Context{
		Reconciler:          pgadapter.NewReconcilerStore(sysPool),
		OutboxRunner:        outboxRunner,
		OutboxRetentionDays: 8,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, res.Attempted, "no rows attempted on empty table")
	assert.Equal(t, 0, res.Succeeded, "no rows succeeded on empty table")
}

// ── P18-REC-TRIAL-001 ───────────────────────────────────────────────────────

// TestREC_TRIAL_001_HardDeletesExpiredPastGrace — a trial_expired
// tenant with trial_ends_at past grace hard-deletes; one within grace and
// one non-trial-expired both survive.
func TestREC_TRIAL_001_HardDeletesExpiredPastGrace(t *testing.T) {
	t.Parallel()
	_, rawPool, sysPool := setupTestDB(t)
	ctx := context.Background()

	past := seedTenantStatus(t, ctx, rawPool, "trial-past-018", "trial_expired", "-30 days")
	within := seedTenantStatus(t, ctx, rawPool, "trial-within-018", "trial_expired", "-5 days")
	activeTrial := seedTenantStatus(t, ctx, rawPool, "trial-active-018", "trial", "+15 days")

	res, err := jobs.TrialCleanup(ctx, &jobs.Context{
		Reconciler:     pgadapter.NewReconcilerStore(sysPool),
		TrialGraceDays: 15,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Succeeded, "only the past-grace trial_expired tenant must delete")

	assert.False(t, tenantExists(t, ctx, rawPool, past), "past-grace tenant must be hard-deleted")
	assert.True(t, tenantExists(t, ctx, rawPool, within), "within-grace tenant must survive")
	assert.True(t, tenantExists(t, ctx, rawPool, activeTrial), "still-active trial must survive")
}

// ── P18-REC-REALM-001 ───────────────────────────────────────────────────────

// TestREC_REALM_001_SweepClearsMarkerOnSuccess — a tenant with
// realm_sync_pending=true is swept, RP.PatchRealmConfig is called with
// the current LocalAccountsEnabled value, and the marker is cleared.
func TestREC_REALM_001_SweepClearsMarkerOnSuccess(t *testing.T) {
	t.Parallel()
	_, rawPool, sysPool := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "realm-sync-018")
	_, err := rawPool.Exec(ctx,
		`UPDATE tenants SET realm_sync_pending = true, local_accounts_enabled = false WHERE id = $1`,
		tenantID)
	require.NoError(t, err)

	rp := &recFakeRP{}
	res, err := jobs.RealmConfigSync(ctx, &jobs.Context{
		Reconciler:       pgadapter.NewReconcilerStore(sysPool),
		Invitations:      pgadapter.NewInvitationRepository(sysPool),
		BatchLimit:       10,
		RealmProvisioner: rp,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Succeeded)
	assert.Equal(t, 0, res.Failed)

	require.Len(t, rp.patchCalls, 1, "RP.PatchRealmConfig must be called exactly once")
	require.NotNil(t, rp.patchCalls[0].LocalAccountsEnabled)
	assert.False(t, *rp.patchCalls[0].LocalAccountsEnabled,
		"disable direction (false) must be pushed to RP")

	// Marker cleared.
	var pending bool
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT realm_sync_pending FROM tenants WHERE id = $1`, tenantID).Scan(&pending))
	assert.False(t, pending, "realm_sync_pending must be cleared after successful sync")
}

// TestREC_REALM_002_MarkerRemainsOnRPFailure — RP.PatchRealmConfig
// returns an error → marker stays set for next tick.
func TestREC_REALM_002_MarkerRemainsOnRPFailure(t *testing.T) {
	t.Parallel()
	_, rawPool, sysPool := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "realm-sync-fail-018")
	_, err := rawPool.Exec(ctx,
		`UPDATE tenants SET realm_sync_pending = true WHERE id = $1`, tenantID)
	require.NoError(t, err)

	rp := &recFakeRP{patchErr: errors.New("simulated RP outage")}
	res, err := jobs.RealmConfigSync(ctx, &jobs.Context{
		Reconciler:       pgadapter.NewReconcilerStore(sysPool),
		Invitations:      pgadapter.NewInvitationRepository(sysPool),
		BatchLimit:       10,
		RealmProvisioner: rp,
	})
	require.NoError(t, err, "job function returns nil; failures counted in Result.Failed")
	assert.Equal(t, 1, res.Attempted)
	assert.Equal(t, 1, res.Failed)
	assert.Equal(t, 0, res.Succeeded)

	// Marker still set — next tick will retry.
	var pending bool
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT realm_sync_pending FROM tenants WHERE id = $1`, tenantID).Scan(&pending))
	assert.True(t, pending, "realm_sync_pending must remain set for next tick retry")
}

// TestREC_REALM_003_DisablesPrioritizedUnderBacklog — a backlog larger than
// BatchLimit, mixing enable (true) and disable (false) targets, must pull
// disables into the batch first (security-tightening direction). This is a
// real gap the LLD-vs-code audit found: the doc claimed this prioritization
// existed, but the sweep query had no ORDER BY at all — fixed alongside
// this test.
func TestREC_REALM_003_DisablesPrioritizedUnderBacklog(t *testing.T) {
	t.Parallel()
	_, rawPool, sysPool := setupTestDB(t)
	ctx := context.Background()

	// 3 enables + 2 disables, backlog of 5, BatchLimit 2 — only the 2
	// disables must be picked up by this tick.
	var disables []uuid.UUID
	for i := 0; i < 2; i++ {
		id := seedTenant(t, ctx, rawPool, "realm-disable-"+itoa(i))
		_, err := rawPool.Exec(ctx,
			`UPDATE tenants SET realm_sync_pending = true, local_accounts_enabled = false WHERE id = $1`, id)
		require.NoError(t, err)
		disables = append(disables, id)
	}
	for i := 0; i < 3; i++ {
		id := seedTenant(t, ctx, rawPool, "realm-enable-"+itoa(i))
		_, err := rawPool.Exec(ctx,
			`UPDATE tenants SET realm_sync_pending = true, local_accounts_enabled = true WHERE id = $1`, id)
		require.NoError(t, err)
	}

	rp := &recFakeRP{}
	res, err := jobs.RealmConfigSync(ctx, &jobs.Context{
		Reconciler:       pgadapter.NewReconcilerStore(sysPool),
		Invitations:      pgadapter.NewInvitationRepository(sysPool),
		BatchLimit:       2,
		RealmProvisioner: rp,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, res.Succeeded)

	require.Len(t, rp.patchCalls, 2, "BatchLimit=2 must cap this tick at exactly 2 calls")
	for _, call := range rp.patchCalls {
		require.NotNil(t, call.LocalAccountsEnabled)
		assert.False(t, *call.LocalAccountsEnabled,
			"disables must be prioritized ahead of enables under a backlog exceeding BatchLimit")
	}

	// Both disable-direction tenants must have had their marker cleared;
	// the enable-direction tenants must still be pending for the next tick.
	for _, id := range disables {
		var pending bool
		require.NoError(t, rawPool.QueryRow(ctx,
			`SELECT realm_sync_pending FROM tenants WHERE id = $1`, id).Scan(&pending))
		assert.False(t, pending, "disable-direction tenant must be synced this tick")
	}
}

// ── P18-REC-KC-001 ──────────────────────────────────────────────────────────

// TestREC_KC_001_ClearsMarkerAfterDeleteUserSuccess — invitation with
// kc_cleanup_pending=true and a keycloak_user_id → RP.DeleteUser called,
// marker cleared.
func TestREC_KC_001_ClearsMarkerAfterDeleteUserSuccess(t *testing.T) {
	t.Parallel()
	_, rawPool, sysPool := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "kc-cleanup-018")
	kcUID := uuid.New()
	inviteID := insertInvitationWithKC(t, ctx, rawPool, tenantID, "kc001@example.com", &kcUID, true)

	rp := &recFakeRP{}
	res, err := jobs.InvitationKCCleanup(ctx, &jobs.Context{
		Reconciler:       pgadapter.NewReconcilerStore(sysPool),
		Invitations:      pgadapter.NewInvitationRepository(sysPool),
		BatchLimit:       10,
		RealmProvisioner: rp,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Succeeded)
	require.Len(t, rp.deleteUserCalls, 1, "RP.DeleteUser must be called exactly once")
	assert.Equal(t, kcUID, rp.deleteUserCalls[0], "correct keycloak_user_id must be passed")

	var pending bool
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT kc_cleanup_pending FROM pending_invitations WHERE id = $1`, inviteID).Scan(&pending))
	assert.False(t, pending, "kc_cleanup_pending must be cleared after DeleteUser success")
}

// TestREC_KC_002_SkipsWhenNoKeycloakUserID — a row with the marker
// set but keycloak_user_id IS NULL clears the marker without calling
// RP (nothing to delete on Keycloak side).
func TestREC_KC_002_SkipsWhenNoKeycloakUserID(t *testing.T) {
	t.Parallel()
	_, rawPool, sysPool := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "kc-cleanup-null-018")
	inviteID := insertInvitationWithKC(t, ctx, rawPool, tenantID, "kc002@example.com", nil, true)

	rp := &recFakeRP{}
	res, err := jobs.InvitationKCCleanup(ctx, &jobs.Context{
		Reconciler:       pgadapter.NewReconcilerStore(sysPool),
		Invitations:      pgadapter.NewInvitationRepository(sysPool),
		BatchLimit:       10,
		RealmProvisioner: rp,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Skipped)
	assert.Equal(t, 0, res.Succeeded)
	assert.Empty(t, rp.deleteUserCalls, "RP must NOT be called when keycloak_user_id IS NULL")

	// Marker still gets cleared so the row doesn't stay stuck.
	var pending bool
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT kc_cleanup_pending FROM pending_invitations WHERE id = $1`, inviteID).Scan(&pending))
	assert.False(t, pending, "kc_cleanup_pending must be cleared even when there's nothing to delete")
}

// TestREC_KC_003_MarkerRemainsOnRPFailure — DEL-6 fail-open: RP
// returns an error → marker stays set, no crash, res.Failed = 1.
func TestREC_KC_003_MarkerRemainsOnRPFailure(t *testing.T) {
	t.Parallel()
	_, rawPool, sysPool := setupTestDB(t)
	ctx := context.Background()

	tenantID := seedTenant(t, ctx, rawPool, "kc-cleanup-fail-018")
	kcUID := uuid.New()
	inviteID := insertInvitationWithKC(t, ctx, rawPool, tenantID, "kc003@example.com", &kcUID, true)

	rp := &recFakeRP{deleteUserErr: errors.New("simulated RP outage")}
	res, err := jobs.InvitationKCCleanup(ctx, &jobs.Context{
		Reconciler:       pgadapter.NewReconcilerStore(sysPool),
		Invitations:      pgadapter.NewInvitationRepository(sysPool),
		BatchLimit:       10,
		RealmProvisioner: rp,
	})
	require.NoError(t, err, "job returns nil; failure is in Result.Failed")
	assert.Equal(t, 1, res.Failed)
	assert.Equal(t, 0, res.Succeeded)

	var pending bool
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT kc_cleanup_pending FROM pending_invitations WHERE id = $1`, inviteID).Scan(&pending))
	assert.True(t, pending, "kc_cleanup_pending must remain set for the next tick to retry")
}

// ── local helpers ───────────────────────────────────────────────────────────

func insertOutboxRows(t *testing.T, ctx context.Context, pool *dbseed.Pool, tenantID uuid.UUID, n int, ageInterval string, published bool) []string {
	t.Helper()
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := uuid.NewString()
		ids[i] = id
		_, err := pool.Exec(ctx, `
			INSERT INTO outbox_events (id, event_type, payload, tenant_id, created_at, scheduled_at)
			VALUES ($1, 'DelegationStarted', '{}'::jsonb, $2, now(), now())`,
			id, tenantID.String())
		require.NoError(t, err)
		if published {
			_, err = pool.Exec(ctx,
				`UPDATE outbox_events SET published_at = now() + INTERVAL `+quoteInterval(ageInterval)+` WHERE id = $1`,
				id)
			require.NoError(t, err)
		}
	}
	return ids
}

func quoteInterval(s string) string { return "'" + s + "'" }

func outboxRowExists(t *testing.T, ctx context.Context, pool *dbseed.Pool, id string) bool {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE id = $1`, id).Scan(&n))
	return n == 1
}

func seedTenantStatus(t *testing.T, ctx context.Context, pool *dbseed.Pool, slug, status, trialInterval string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO tenants (id, slug, name, plan, status, trial_ends_at)
		VALUES ($1, $2, $3, 'starter', $4::subscription_status, now() + INTERVAL `+quoteInterval(trialInterval)+`)`,
		id, slug, slug, status)
	require.NoError(t, err)
	return id
}

func tenantExists(t *testing.T, ctx context.Context, pool *dbseed.Pool, id uuid.UUID) bool {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE id = $1`, id).Scan(&n))
	return n == 1
}

func insertInvitationWithKC(t *testing.T, ctx context.Context, pool *dbseed.Pool, tenantID uuid.UUID, email string, kcUserID *uuid.UUID, cleanupPending bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO pending_invitations
			(id, tenant_id, email, full_name, invited_by, keycloak_user_id, kc_cleanup_pending, status, expires_at)
		VALUES ($1, $2, $3, 'Cleanup Test', gen_random_uuid(), $4, $5, 'pending', now() + interval '7 days')`,
		id, tenantID, email, kcUserID, cleanupPending)
	require.NoError(t, err)
	// Once created, transition to revoked so kc_cleanup_pending sweep is realistic
	// (a fresh 'pending' invitation would still be within the accept window).
	_, err = pool.Exec(ctx,
		`UPDATE pending_invitations SET status = 'revoked' WHERE id = $1`, id)
	require.NoError(t, err)
	return id
}

// (helpers above accept *dbseed.Pool directly — no shim types needed.)
