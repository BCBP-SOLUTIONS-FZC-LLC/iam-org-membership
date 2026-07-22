package jobs

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RealmConfigSync sweeps tenants with realm_sync_pending=true (T-15
// Option A local-first + reconcile). For each row:
//  1. Read current desired realm-affecting settings from the tenants row.
//  2. Call RP PatchRealmConfig.
//  3. On success, clear realm_sync_pending.
//  4. On failure, leave marker for next tick.
//
// Disable direction (local_accounts_enabled=false) is prioritized
// (security-tightening) — but at the sweep level we just push the
// current value regardless.
func RealmConfigSync(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	type target struct {
		tenantID             uuid.UUID
		localAccountsEnabled bool
	}
	var targets []target

	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, local_accounts_enabled FROM tenants
			WHERE realm_sync_pending = true AND deleted_at IS NULL
			LIMIT $1`, jctx.BatchLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t target
			if err := rows.Scan(&t.tenantID, &t.localAccountsEnabled); err != nil {
				return err
			}
			targets = append(targets, t)
		}
		return rows.Err()
	})
	if err != nil {
		return res, err
	}

	for _, t := range targets {
		res.Attempted++
		if err := jctx.RealmProvisioner.PatchRealmConfig(ctx, t.tenantID, port.RealmConfigPatch{
			LocalAccountsEnabled: &t.localAccountsEnabled,
		}); err != nil {
			jctx.Logger.Warn("realm-config-sync: RP call failed — leaving marker",
				"tenant_id", t.tenantID, "error", err.Error())
			res.Failed++
			continue
		}
		if err := clearRealmSyncPending(ctx, jctx, t.tenantID); err != nil {
			jctx.Logger.Warn("realm-config-sync: clear marker failed", "tenant_id", t.tenantID, "error", err.Error())
			res.Failed++
			continue
		}
		res.Succeeded++
	}
	jctx.Logger.Info("realm-config-sync complete",
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed)
	return res, nil
}

func clearRealmSyncPending(ctx context.Context, jctx *Context, tenantID uuid.UUID) error {
	return runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET realm_sync_pending = false WHERE id = $1`, tenantID)
		return err
	})
}
