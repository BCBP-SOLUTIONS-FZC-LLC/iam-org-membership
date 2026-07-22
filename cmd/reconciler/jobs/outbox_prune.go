package jobs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// OutboxPrune deletes outbox_events rows that have been successfully
// published + acknowledged past the retention window (default 8 days;
// the outbox runner's own PrunePublished writes to a status column).
//
// We implement this directly against the outbox_events schema (created by
// platform-events/outbox.ApplySchema at startup): the runner marks rows
// with published_at IS NOT NULL after successful SNS publish; anything
// older than the retention window is safe to drop.
func OutboxPrune(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, fmt.Sprintf(`
			DELETE FROM outbox_events
			WHERE published_at IS NOT NULL
			  AND published_at < now() - INTERVAL '%d days'`, jctx.OutboxRetentionDays))
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
	jctx.Logger.Info("outbox-prune complete",
		"deleted", res.Succeeded, "retention_days", jctx.OutboxRetentionDays)
	return res, nil
}
