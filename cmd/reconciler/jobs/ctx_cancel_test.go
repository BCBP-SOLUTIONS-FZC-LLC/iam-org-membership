package jobs

import (
	"context"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each sweep checks ctx.Err() before every row (OutboxPrune: before every
// further batch). With an already-cancelled context the job must return the
// context error at that check and make no per-row call. The fakes embed nil
// port interfaces, so any call they do not implement panics and fails the test.

func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// ── invitation-kc-cleanup ─────────────────────────────────────────────

type cancelKCInvites struct {
	port.InvitationRepository
	targets []domain.PendingInvitation
	cleared int
}

func (f *cancelKCInvites) ListPendingKCCleanup(context.Context, int) ([]domain.PendingInvitation, error) {
	return f.targets, nil
}

func (f *cancelKCInvites) ClearKCCleanupPendingByID(context.Context, uuid.UUID) error {
	f.cleared++
	return nil
}

type cancelKCRealm struct {
	port.RealmProvisionerClient
	deletes int
}

func (f *cancelKCRealm) DeleteUser(context.Context, uuid.UUID, uuid.UUID) error {
	f.deletes++
	return nil
}

func TestInvitationKCCleanup_CancelledContext_StopsBeforeFirstRow(t *testing.T) {
	kc := uuid.New()
	invites := &cancelKCInvites{targets: []domain.PendingInvitation{
		{ID: uuid.New(), TenantID: uuid.New(), KeycloakUserID: &kc},
		{ID: uuid.New(), TenantID: uuid.New()},
	}}
	rp := &cancelKCRealm{}

	res, err := InvitationKCCleanup(cancelledCtx(), &Context{Invitations: invites, RealmProvisioner: rp, Logger: pruneLogger(), BatchLimit: 10})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, Result{}, res)
	assert.Zero(t, rp.deletes, "no RP DeleteUser after cancellation")
	assert.Zero(t, invites.cleared, "no marker cleared after cancellation")
}

// ── outbox-prune ──────────────────────────────────────────────────────

func TestOutboxPrune_CancelledContext_StopsAfterFirstFullBatch(t *testing.T) {
	pruner := &batchPruner{remaining: 1500}

	res, err := OutboxPrune(cancelledCtx(), &Context{OutboxRunner: pruner, BatchLimit: 500, Logger: pruneLogger()})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, pruner.calls, "the check runs after a full batch: one batch, then stop")
	assert.Equal(t, Result{Attempted: 500, Succeeded: 500}, res)
}

// ── parked-events-replay ──────────────────────────────────────────────

func TestParkedEventsReplay_CancelledContext_StopsBeforeFirstTenant(t *testing.T) {
	replayer := &fakeParkedReplayer{}

	res, err := ParkedEventsReplay(cancelledCtx(), &Context{
		Reconciler:     &fakeParkedListStore{ids: []uuid.UUID{uuid.New(), uuid.New()}},
		ParkedReplayer: replayer,
		Logger:         pruneLogger(),
		BatchLimit:     10,
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, Result{}, res)
	assert.Empty(t, replayer.calls, "no tenant replayed after cancellation")
}

// ── realm-config-sync ─────────────────────────────────────────────────

type cancelRealmPatch struct {
	port.RealmProvisionerClient
	patches int
}

func (f *cancelRealmPatch) PatchRealmConfig(context.Context, uuid.UUID, port.RealmConfigPatch) error {
	f.patches++
	return nil
}

func TestRealmConfigSync_CancelledContext_StopsBeforeFirstTenant(t *testing.T) {
	jctx, reconciler, _, m := newRealmSyncTestContext()
	reconciler.candidates = []port.RealmSyncCandidate{{TenantID: uuid.New()}, {TenantID: uuid.New()}}
	rp := &cancelRealmPatch{}
	jctx.RealmProvisioner = rp

	res, err := RealmConfigSync(cancelledCtx(), jctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, Result{}, res)
	assert.Zero(t, rp.patches, "no RP PatchRealmConfig after cancellation")
	assert.Empty(t, reconciler.clearedIDs, "no marker cleared after cancellation")
	assert.Empty(t, m.realmSyncFailedStages)
}

// ── trial-cleanup ─────────────────────────────────────────────────────

type cancelTrialStore struct {
	port.ReconcilerStore
	ids []uuid.UUID
}

func (f *cancelTrialStore) ListExpiredTrialTenants(context.Context, int, int) ([]uuid.UUID, error) {
	return f.ids, nil
}

type countingTxRunner struct{ runs int }

func (r *countingTxRunner) RunInTx(ctx context.Context, fn func(context.Context) error) error {
	r.runs++
	return fn(ctx)
}

func TestTrialCleanup_CancelledContext_StopsBeforeFirstTenant(t *testing.T) {
	tx := &countingTxRunner{}

	res, err := TrialCleanup(cancelledCtx(), &Context{
		Reconciler:     &cancelTrialStore{ids: []uuid.UUID{uuid.New(), uuid.New()}},
		TxRunner:       tx,
		Logger:         pruneLogger(),
		BatchLimit:     10,
		TrialGraceDays: 15,
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, Result{}, res)
	assert.Zero(t, tx.runs, "no hard-delete transaction after cancellation")
}
