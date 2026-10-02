package jobs

import (
	"context"
	"testing"
	"time"

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

// batchPruner returns full batches until remaining runs out.
type batchPruner struct {
	remaining int64
	calls     int
	limits    []int
}

func (p *batchPruner) PrunePublished(_ context.Context, _ time.Duration, limit int) (int64, error) {
	p.calls++
	p.limits = append(p.limits, limit)
	n := min(p.remaining, int64(limit))
	p.remaining -= n
	return n, nil
}

// PrunePublished deletes at most one batch per call; one call per daily run
// would cap pruning at one batch a day, so OutboxPrune loops until a batch
// comes back short.
func TestOutboxPrune_LoopsUntilBacklogCleared(t *testing.T) {
	pruner := &batchPruner{remaining: 1250}
	jctx := &Context{OutboxRunner: pruner, BatchLimit: 500, Logger: port.NewSlogStyleLogger(&warnCapture{})}

	res, err := OutboxPrune(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, 1250, res.Succeeded)
	assert.Equal(t, 3, pruner.calls, "500 + 500 + 250, stopping on the short batch")
	assert.Equal(t, []int{500, 500, 500}, pruner.limits)
}

func TestOutboxPrune_ExactMultipleStopsOnEmptyBatch(t *testing.T) {
	pruner := &batchPruner{remaining: 1000}
	res, err := OutboxPrune(context.Background(), &Context{OutboxRunner: pruner, BatchLimit: 500, Logger: port.NewSlogStyleLogger(&warnCapture{})})
	require.NoError(t, err)
	assert.Equal(t, 1000, res.Succeeded)
	assert.Equal(t, 3, pruner.calls)
}

type errPruner struct{}

func (errPruner) PrunePublished(context.Context, time.Duration, int) (int64, error) {
	return 0, assert.AnError
}

func TestOutboxPrune_ErrorStops(t *testing.T) {
	_, err := OutboxPrune(context.Background(), &Context{OutboxRunner: errPruner{}, BatchLimit: 10, Logger: port.NewSlogStyleLogger(&warnCapture{})})
	require.ErrorIs(t, err, assert.AnError)
}

type recordingInboxPruner struct {
	consumers []string
	retention time.Duration
	batch     int
	err       error
}

func (p *recordingInboxPruner) Prune(_ context.Context, consumer string, retention time.Duration, batch int) (int64, error) {
	p.consumers = append(p.consumers, consumer)
	p.retention, p.batch = retention, batch
	return 3, p.err
}

func TestProcessedEventsPrune_PrunesEveryConsumer(t *testing.T) {
	pruner := &recordingInboxPruner{}
	jctx := &Context{
		Inbox: pruner, InboxConsumers: []string{"iam-org-membership", "catalog"},
		BatchLimit: 500, ProcessedEventsTTLDays: 8, Logger: port.NewSlogStyleLogger(&warnCapture{}),
	}
	res, err := ProcessedEventsPrune(context.Background(), jctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"iam-org-membership", "catalog"}, pruner.consumers)
	assert.Equal(t, 8*24*time.Hour, pruner.retention)
	assert.Equal(t, 500, pruner.batch)
	assert.Equal(t, 6, res.Succeeded)
}

func TestProcessedEventsPrune_DefaultsRetentionAndStopsOnError(t *testing.T) {
	pruner := &recordingInboxPruner{err: assert.AnError}
	jctx := &Context{Inbox: pruner, InboxConsumers: []string{"a", "b"}, Logger: port.NewSlogStyleLogger(&warnCapture{})}
	_, err := ProcessedEventsPrune(context.Background(), jctx)
	require.ErrorIs(t, err, assert.AnError)
	assert.Equal(t, []string{"a"}, pruner.consumers)
	assert.Equal(t, 8*24*time.Hour, pruner.retention, "unset TTL defaults to PE-1's 8 days")
}

func TestProcessedEventsPrune_NilInbox_SkipsAndWarns(t *testing.T) {
	logger := &warnCapture{}
	res, err := ProcessedEventsPrune(context.Background(), &Context{Logger: port.NewSlogStyleLogger(logger)})
	require.NoError(t, err)
	assert.Equal(t, Result{}, res)
	require.NotEmpty(t, logger.msgs)
}
