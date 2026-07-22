package jobs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TrialCleanup hard-deletes trial_expired tenants past the grace window
// (§8.10.3, default 15 days). Trial lifecycle terminates here — paid
// tenants never hard-delete (PAID-1 terminal at 'offboarded' with
// soft-delete + PII scrub).
//
// Row selection: status='trial_expired' AND trial_ends_at < now() - grace.
// Child rows cascade via ON DELETE CASCADE (§4.2). trial_signup_ledger is
// retained (TRIAL-2 one-lifetime-trial).
func TrialCleanup(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, fmt.Sprintf(`
			DELETE FROM tenants
			WHERE status = 'trial_expired'
			  AND trial_ends_at < now() - INTERVAL '%d days'
			  AND deleted_at IS NULL`, jctx.TrialGraceDays))
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
	jctx.Logger.Info("trial-cleanup complete",
		"deleted", res.Succeeded, "grace_days", jctx.TrialGraceDays)
	return res, nil
}
