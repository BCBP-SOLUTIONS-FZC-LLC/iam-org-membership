package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeReconcilerStore backs only the two methods RealmConfigSync calls; the
// rest panic if exercised so a future caller expansion fails loudly instead
// of silently returning a zero value.
type fakeReconcilerStore struct {
	port.ReconcilerStore
	candidates []port.RealmSyncCandidate
	clearErr   error
	clearedIDs []uuid.UUID
	listErr    error

	pruneOutboxN   int
	pruneOutboxErr error
}

func (f *fakeReconcilerStore) ListRealmSyncPending(context.Context, int) ([]port.RealmSyncCandidate, error) {
	return f.candidates, f.listErr
}

func (f *fakeReconcilerStore) ClearRealmSyncPending(_ context.Context, tenantID uuid.UUID) error {
	f.clearedIDs = append(f.clearedIDs, tenantID)
	return f.clearErr
}

func (f *fakeReconcilerStore) PruneOutbox(context.Context, int, int) (int, error) {
	return f.pruneOutboxN, f.pruneOutboxErr
}

// fakeRealmProvisioner backs only PatchRealmConfig; the rest panic if
// exercised, same rationale as fakeReconcilerStore.
type fakeRealmProvisioner struct {
	port.RealmProvisionerClient
	patchErr error
}

func (f *fakeRealmProvisioner) PatchRealmConfig(context.Context, uuid.UUID, port.RealmConfigPatch) error {
	return f.patchErr
}

// fakeJobMetrics records every IncRealmSyncFailed call so tests can assert
// on the exact stage label without touching the real Prometheus registry.
type fakeJobMetrics struct {
	realmSyncFailedStages []string
}

func (f *fakeJobMetrics) IncRealmSyncFailed(stage string) {
	f.realmSyncFailedStages = append(f.realmSyncFailedStages, stage)
}

func newRealmSyncTestContext() (*Context, *fakeReconcilerStore, *fakeRealmProvisioner, *fakeJobMetrics) {
	reconciler := &fakeReconcilerStore{}
	rp := &fakeRealmProvisioner{}
	m := &fakeJobMetrics{}
	jctx := &Context{
		Reconciler:       reconciler,
		RealmProvisioner: rp,
		Logger:           port.NewSlogStyleLogger(&warnCapture{}),
		Metrics:          m,
		BatchLimit:       500,
	}
	return jctx, reconciler, rp, m
}

func TestRealmConfigSync_PatchFails_IncrementsMetricWithPatchStage(t *testing.T) {
	jctx, reconciler, rp, m := newRealmSyncTestContext()
	reconciler.candidates = []port.RealmSyncCandidate{{TenantID: uuid.New(), LocalAccountsEnabled: false}}
	rp.patchErr = errors.New("rp unavailable")

	res, err := RealmConfigSync(context.Background(), jctx)

	require.NoError(t, err, "per-row errors must not fail the whole run")
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	require.Equal(t, []string{"patch_realm_config"}, m.realmSyncFailedStages)
	assert.Empty(t, reconciler.clearedIDs, "ClearRealmSyncPending must not be called after a PatchRealmConfig failure")
}

func TestRealmConfigSync_ClearMarkerFails_IncrementsMetricWithClearMarkerStage(t *testing.T) {
	jctx, reconciler, _, m := newRealmSyncTestContext()
	reconciler.candidates = []port.RealmSyncCandidate{{TenantID: uuid.New(), LocalAccountsEnabled: true}}
	reconciler.clearErr = errors.New("db unavailable")

	res, err := RealmConfigSync(context.Background(), jctx)

	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	require.Equal(t, []string{"clear_marker"}, m.realmSyncFailedStages)
}

func TestRealmConfigSync_HappyPath_NoMetricIncrement(t *testing.T) {
	jctx, _, _, m := newRealmSyncTestContext()
	jctx.Reconciler.(*fakeReconcilerStore).candidates = []port.RealmSyncCandidate{
		{TenantID: uuid.New(), LocalAccountsEnabled: true},
	}

	res, err := RealmConfigSync(context.Background(), jctx)

	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res)
	assert.Empty(t, m.realmSyncFailedStages)
}

func TestRealmConfigSync_NilMetrics_DoesNotPanic(t *testing.T) {
	jctx, reconciler, rp, _ := newRealmSyncTestContext()
	jctx.Metrics = nil // nil-safe per Context.Metrics doc comment
	reconciler.candidates = []port.RealmSyncCandidate{{TenantID: uuid.New()}}
	rp.patchErr = errors.New("rp unavailable")

	assert.NotPanics(t, func() {
		res, err := RealmConfigSync(context.Background(), jctx)
		require.NoError(t, err)
		assert.Equal(t, Result{Attempted: 1, Failed: 1}, res)
	})
}

func TestRealmConfigSync_ListError_PropagatesAndSkipsMetrics(t *testing.T) {
	jctx, reconciler, _, m := newRealmSyncTestContext()
	reconciler.listErr = errors.New("db unavailable")

	res, err := RealmConfigSync(context.Background(), jctx)

	require.Error(t, err)
	assert.Equal(t, Result{}, res)
	assert.Empty(t, m.realmSyncFailedStages)
}
