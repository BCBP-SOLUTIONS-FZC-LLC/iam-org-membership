package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type warnCapture struct {
	msgs []string
}

func (w *warnCapture) Debug(string, map[string]any) {}
func (w *warnCapture) Info(string, map[string]any)  {}
func (w *warnCapture) Error(string, map[string]any) {}
func (w *warnCapture) Warn(msg string, _ map[string]any) {
	w.msgs = append(w.msgs, msg)
}

// TestOutboxPrune_HappyPath_ReturnsDeletedCount replaces a previous version
// of this test that constructed a Context with a nil Reconciler and
// expected a graceful "no outbox.Runner wired" skip — a contract that
// predates jctx.Reconciler.PruneOutbox and was never actually implemented
// (Reconciler is a required dependency, always wired by
// cmd/reconciler/main.go, and no other job in this package nil-checks its
// port dependencies before use). That mismatch made the real OutboxPrune
// panic on a nil Reconciler instead of the warn-and-skip the old test
// asserted. This file now tests OutboxPrune's actual, current contract.
func TestOutboxPrune_HappyPath_ReturnsDeletedCount(t *testing.T) {
	reconciler := &fakeReconcilerStore{pruneOutboxN: 42}
	jctx := &Context{
		Reconciler:          reconciler,
		Logger:              port.NewSlogStyleLogger(&warnCapture{}),
		OutboxRetentionDays: 8,
		BatchLimit:          500,
	}

	res, err := OutboxPrune(context.Background(), jctx)

	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 42, Succeeded: 42}, res)
}

func TestOutboxPrune_BatchLimitUnset_UsesDefault(t *testing.T) {
	reconciler := &fakeReconcilerStore{pruneOutboxN: 1}
	jctx := &Context{
		Reconciler: reconciler,
		Logger:     port.NewSlogStyleLogger(&warnCapture{}),
		// BatchLimit deliberately left at the zero value.
	}

	res, err := OutboxPrune(context.Background(), jctx)

	require.NoError(t, err)
	assert.Equal(t, Result{Attempted: 1, Succeeded: 1}, res)
}

func TestOutboxPrune_StoreError_Propagates(t *testing.T) {
	reconciler := &fakeReconcilerStore{pruneOutboxErr: errors.New("db unavailable")}
	jctx := &Context{
		Reconciler: reconciler,
		Logger:     port.NewSlogStyleLogger(&warnCapture{}),
	}

	res, err := OutboxPrune(context.Background(), jctx)

	require.Error(t, err)
	assert.Equal(t, Result{}, res)
}
