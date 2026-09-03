package jobs

import (
	"context"
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
	n, err := jctx.Reconciler.HardDeleteExpiredTrials(ctx, jctx.TrialGraceDays)
	if err != nil {
		return res, err
	}
	res.Attempted = n
	res.Succeeded = n
	jctx.Logger.Info("trial-cleanup complete",
		"deleted", res.Succeeded, "grace_days", jctx.TrialGraceDays)
	return res, nil
}
