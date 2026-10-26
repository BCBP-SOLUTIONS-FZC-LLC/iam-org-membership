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

type fakeParkedListStore struct {
	port.ReconcilerStore
	ids     []uuid.UUID
	listErr error
}

func (f *fakeParkedListStore) ListTenantsWithParkedEvents(context.Context, int) ([]uuid.UUID, error) {
	return f.ids, f.listErr
}

type fakeParkedReplayer struct {
	failFor map[uuid.UUID]bool
	calls   []uuid.UUID
}

func (f *fakeParkedReplayer) ReplayParkedSweep(_ context.Context, tenantID uuid.UUID) (int, error) {
	f.calls = append(f.calls, tenantID)
	if f.failFor[tenantID] {
		return 0, errors.New("replay failed")
	}
	return 1, nil
}

func TestParkedEventsReplay_ReplaysEachTenantAndCountsFailures(t *testing.T) {
	ok, bad := uuid.New(), uuid.New()
	replayer := &fakeParkedReplayer{failFor: map[uuid.UUID]bool{bad: true}}
	jctx := &Context{
		Reconciler:     &fakeParkedListStore{ids: []uuid.UUID{ok, bad}},
		ParkedReplayer: replayer,
		Logger:         port.NewSlogStyleLogger(nil),
		BatchLimit:     10,
	}

	res, err := ParkedEventsReplay(context.Background(), jctx)
	require.NoError(t, err, "a per-tenant failure does not fail the run")
	assert.Equal(t, Result{Attempted: 2, Succeeded: 1, Failed: 1}, res)
	assert.Equal(t, []uuid.UUID{ok, bad}, replayer.calls)
}

func TestParkedEventsReplay_ListError_FailsRun(t *testing.T) {
	jctx := &Context{
		Reconciler:     &fakeParkedListStore{listErr: errors.New("db down")},
		ParkedReplayer: &fakeParkedReplayer{},
		Logger:         port.NewSlogStyleLogger(nil),
	}
	_, err := ParkedEventsReplay(context.Background(), jctx)
	require.Error(t, err)
}

func TestParkedEventsReplay_NoReplayer_Skips(t *testing.T) {
	res, err := ParkedEventsReplay(context.Background(), &Context{Logger: port.NewSlogStyleLogger(nil)})
	require.NoError(t, err)
	assert.Equal(t, Result{}, res)
}
