package jobs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ProcessedEventsPrune deletes processed_events rows older than the
// retention window (PE-1: default 8 days, strictly > 7-day SQS lifetime;
// IDEMP-4 backstopped by EVT-14/PI-10/IDEMP-3 for beyond-window duplicates).
//
// idx_processed_events_prune (processed_at) makes the inner SELECT below an
// indexed range scan. Batched by jctx.BatchLimit for the same reason as
// OutboxPrune — a backlog larger than one tick converges over several
// ticks instead of one unbounded DELETE holding locks across every stale row.
func ProcessedEventsPrune(ctx context.Context, jctx *Context) (Result, error) {
	batchLimit := jctx.BatchLimit
	if batchLimit <= 0 {
		batchLimit = defaultPruneBatchLimit
	}
	var res Result
	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, fmt.Sprintf(`
			DELETE FROM processed_events
			WHERE (event_id, consumer) IN (
				SELECT event_id, consumer FROM processed_events
				WHERE processed_at < now() - INTERVAL '%d days'
				LIMIT $1
			)`, jctx.ProcessedEventsTTLDays), batchLimit)
		if err != nil {
			return err
		}
		n := int(cmd.RowsAffected())
		res.Attempted = n
		res.Succeeded = n
		return nil
	})
	if err != nil {
		return res, err
	}
	jctx.Logger.Info("processed-events-prune complete",
		"deleted", res.Succeeded, "ttl_days", jctx.ProcessedEventsTTLDays)
	return res, nil
}
