package jobs

import (
	"context"
	"time"
)

// defaultPruneBatchLimit is used by OutboxPrune/ProcessedEventsPrune when
// jctx.BatchLimit is unset (<=0) — matches RECONCILER_BATCH_LIMIT's own
// production default (cmd/reconciler/main.go) so a caller that never set
// BatchLimit still gets a real, bounded batch rather than deleting 0 rows
// (LIMIT 0) or falling back to an unbounded delete.
const defaultPruneBatchLimit = 500

// OutboxPrune deletes published outbox_events rows older than the
// retention window via platform-events' outbox.Runner.PrunePublished —
// the library's own batched DELETE (same SQL the runner uses in-process).
// Retention defaults to 8 days (OUTBOX_RETENTION_DAYS). PrunePublished
// deletes at most one batch (RECONCILER_BATCH_LIMIT) per call, so it is
// called until a batch comes back short: a single call per daily run would
// cap pruning at one batch a day and let the table grow without bound.
func OutboxPrune(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	if jctx.OutboxRunner == nil {
		jctx.Logger.Warn("outbox-prune: no outbox.Runner wired — skipping")
		return res, nil
	}
	batchLimit := jctx.BatchLimit
	if batchLimit <= 0 {
		batchLimit = defaultPruneBatchLimit
	}
	retentionDays := jctx.OutboxRetentionDays
	if retentionDays <= 0 {
		retentionDays = 8
	}
	retention := time.Duration(retentionDays) * 24 * time.Hour
	for {
		n, err := jctx.OutboxRunner.PrunePublished(ctx, retention, batchLimit)
		res.Attempted += int(n)
		res.Succeeded += int(n)
		if err != nil {
			return res, err
		}
		if n < int64(batchLimit) {
			break
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
	jctx.Logger.Info("outbox-prune complete",
		"deleted", res.Succeeded, "retention_days", retentionDays)
	return res, nil
}
