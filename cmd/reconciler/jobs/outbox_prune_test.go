package jobs

import (
	"context"
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

// OutboxPrune wraps platform-events' outbox.Runner.PrunePublished, a
// concrete type this repo does not own — only the nil-OutboxRunner
// skip-and-WARN branch is this repo's own code to test.
func TestOutboxPrune_NilRunner_SkipsAndWarns(t *testing.T) {
	logger := &warnCapture{}
	jctx := &Context{
		OutboxRunner: nil,
		Logger:       port.NewSlogStyleLogger(logger),
	}

	res, err := OutboxPrune(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{}, res)
	require.NotEmpty(t, logger.msgs)
	assert.Contains(t, logger.msgs[0], "no outbox.Runner wired")
}
