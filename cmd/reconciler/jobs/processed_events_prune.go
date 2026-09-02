package jobs

import (
	"context"
)

// ProcessedEventsPrune deletes processed_events rows older than the
// retention window (PE-1: default 8 days, strictly > 7-day SQS lifetime;
// IDEMP-4 backstopped by EVT-14/PI-10/IDEMP-3 for beyond-window duplicates).
//
// idx_processed_events_prune (processed_at) makes the inner SELECT an
// indexed range scan. Batched by jctx.BatchLimit for the same reason as
// OutboxPrune — a backlog larger than one tick converges over several
// ticks instead of one unbounded DELETE holding locks across every stale row.
func ProcessedEventsPrune(ctx context.Context, jctx *Context) (Result, error) {
	batchLimit := jctx.BatchLimit
	if batchLimit <= 0 {
		batchLimit = defaultPruneBatchLimit
	}
	var res Result
	n, err := jctx.Reconciler.PruneProcessedEvents(ctx, jctx.ProcessedEventsTTLDays, batchLimit)
	if err != nil {
		return res, err
	}
	res.Attempted = n
	res.Succeeded = n
	jctx.Logger.Info("processed-events-prune complete",
		"deleted", res.Succeeded, "ttl_days", jctx.ProcessedEventsTTLDays)
	return res, nil
}
