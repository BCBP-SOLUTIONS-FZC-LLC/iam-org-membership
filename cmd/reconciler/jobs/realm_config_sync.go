package jobs

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
)

// RealmConfigSync sweeps tenants with realm_sync_pending=true (T-15
// Option A local-first + reconcile). For each row:
//  1. Read current desired realm-affecting settings from the tenants row.
//  2. Call RP PatchRealmConfig.
//  3. On success, clear realm_sync_pending.
//  4. On failure, leave marker for next tick.
//
// Disable direction (local_accounts_enabled=false) is prioritized
// (security-tightening): the sweep query orders disables first, so under a
// backlog larger than BatchLimit, un-applied disables are the ones pulled
// into this tick rather than being starved behind a run of enables.
func RealmConfigSync(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	targets, err := jctx.Reconciler.ListRealmSyncPending(ctx, jctx.BatchLimit)
	if err != nil {
		return res, err
	}

	for _, t := range targets {
		res.Attempted++
		if err := jctx.RealmProvisioner.PatchRealmConfig(ctx, t.TenantID, port.RealmConfigPatch{
			LocalAccountsEnabled: &t.LocalAccountsEnabled,
		}); err != nil {
			jctx.Logger.Warn("realm-config-sync: RP call failed — leaving marker",
				"tenant_id", t.TenantID, "error", err.Error())
			res.Failed++
			if jctx.Metrics != nil {
				jctx.Metrics.IncRealmSyncFailed("patch_realm_config")
			}
			continue
		}
		if err := jctx.Reconciler.ClearRealmSyncPending(ctx, t.TenantID); err != nil {
			jctx.Logger.Warn("realm-config-sync: clear marker failed", "tenant_id", t.TenantID, "error", err.Error())
			res.Failed++
			if jctx.Metrics != nil {
				jctx.Metrics.IncRealmSyncFailed("clear_marker")
			}
			continue
		}
		res.Succeeded++
	}
	jctx.Logger.Info("realm-config-sync complete",
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed)
	return res, nil
}
