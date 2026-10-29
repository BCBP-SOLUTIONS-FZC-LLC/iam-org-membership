package jobs

import (
	"context"
	"time"
)

// defaultTrialLedgerRetentionDays bounds how long a trial_signup_ledger row
// (the HMAC of a trial owner's email) is kept: long enough for the
// one-trial-per-email rule (TRIAL-2) to mean something, short enough that
// the pseudonymous hash is not held forever (GDPR storage limitation).
const defaultTrialLedgerRetentionDays = 730

// TrialLedgerPrune deletes trial_signup_ledger rows older than the
// retention window (TRIAL_LEDGER_RETENTION_DAYS, default 730), in batches of
// jctx.BatchLimit until none is left. A pruned email can start a new trial.
func TrialLedgerPrune(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	if jctx.TrialLedger == nil {
		jctx.Logger.Warn("trial-ledger-prune: no ledger wired — skipping")
		return res, nil
	}
	days := jctx.TrialLedgerRetentionDays
	if days <= 0 {
		days = defaultTrialLedgerRetentionDays
	}
	limit := jctx.BatchLimit
	if limit <= 0 {
		limit = defaultPruneBatchLimit
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	for {
		n, err := jctx.TrialLedger.PruneCreatedBefore(ctx, cutoff, limit)
		res.Attempted += int(n)
		res.Succeeded += int(n)
		if err != nil {
			return res, err
		}
		if n < int64(limit) {
			break
		}
	}
	jctx.Logger.Info("trial-ledger-prune complete", "deleted", res.Succeeded, "retention_days", days)
	return res, nil
}
