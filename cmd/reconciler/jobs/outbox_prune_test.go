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

func TestOutboxPrune_NilRunner_SkipsAndWarns(t *testing.T) {
	log := &warnCapture{}
	jctx := &Context{Logger: port.NewSlogStyleLogger(log)}

	res, err := OutboxPrune(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, Result{}, res)
	require.NotEmpty(t, log.msgs)
	assert.Contains(t, log.msgs[0], "no outbox.Runner wired")
}
