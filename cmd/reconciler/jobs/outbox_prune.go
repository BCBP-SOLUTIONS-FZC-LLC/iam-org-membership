package jobs

import (
	"context"
)

// defaultPruneBatchLimit is used by OutboxPrune/ProcessedEventsPrune when
// jctx.BatchLimit is unset (<=0) — matches RECONCILER_BATCH_LIMIT's own
// production default (cmd/reconciler/main.go) so a caller that never set
// BatchLimit still gets a real, bounded batch rather than deleting 0 rows
// (LIMIT 0) or falling back to an unbounded delete.
const defaultPruneBatchLimit = 500

// OutboxPrune deletes outbox_events rows that have been successfully
// published + acknowledged past the retention window (default 8 days;
// the outbox runner's own PrunePublished writes to a status column).
//
// We implement this directly against the outbox_events schema (created by
// platform-events/outbox.ApplySchema at startup): the runner marks rows
// with published_at IS NOT NULL after successful SNS publish; anything
// older than the retention window is safe to drop.
//
// Batched by jctx.BatchLimit (subquery + LIMIT, then DELETE ... WHERE id IN
// (...)) rather than one unbounded DELETE — closes a real gap the
// LLD-vs-code audit found: a backlog larger than one tick's worth would
// otherwise produce a single long-running DELETE holding row locks across
// however many rows exist, instead of converging over several ticks like
// every other batched reconciler in this package.
func OutboxPrune(ctx context.Context, jctx *Context) (Result, error) {
	batchLimit := jctx.BatchLimit
	if batchLimit <= 0 {
		batchLimit = defaultPruneBatchLimit
	}
	var res Result
	n, err := jctx.Reconciler.PruneOutbox(ctx, jctx.OutboxRetentionDays, batchLimit)
	if err != nil {
		return res, err
	}
	res.Attempted = n
	res.Succeeded = n
	jctx.Logger.Info("outbox-prune complete",
		"deleted", res.Succeeded, "retention_days", jctx.OutboxRetentionDays)
	return res, nil
}
