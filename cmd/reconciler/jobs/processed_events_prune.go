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
// idx_processed_events_prune (processed_at) makes this a single indexed
// range delete.
func ProcessedEventsPrune(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, fmt.Sprintf(`
			DELETE FROM processed_events
			WHERE processed_at < now() - INTERVAL '%d days'`, jctx.ProcessedEventsTTLDays))
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
