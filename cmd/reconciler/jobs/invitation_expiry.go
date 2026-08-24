package jobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// InvitationExpiry flips pending invitations past expires_at to 'expired'
// (§13.1, PI-5). Frees a seat back into the SEAT-1 count on the next
// membership-add. Idempotent — restart re-selects only rows still in
// 'pending' state.
//
// Also sets kc_cleanup_pending=true (PI-9) so the never-activated Keycloak
// user backing an expired invitation is durably scheduled for deletion by
// the invitation-kc-cleanup reconciler — an expired invite's shell account
// is not left orphaned.
//
// Uses sysPool (BYPASSRLS) because the sweep spans tenants.
//
// Note on wave-expiry seat-overage resolution (§8.10.3): this job does
// NOT invalidate `om:seat_usage:<tenant>` caches or emit
// `TenantSeatOverageResolved` when a batch of expiries drops pending
// below the licensed cap. That work is delegated to `seat-overage-
// reconcile`, which sweeps every tenant's overage state daily. Callers
// hitting /seat-usage in the intervening window may see stale numbers
// up to CACHE-5 TTL — an accepted LLD trade-off (avoids an O(tenants)
// broadcast on every expiry tick).
func InvitationExpiry(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	// Select expired pending rows in batches to keep lock hold time bounded.
	// LIMIT via a scoped subquery — plain UPDATE ... LIMIT isn't Postgres
	// syntax. Sibling reconcilers (seat_overage, realm_config_sync,
	// invitation_kc_cleanup) use the same pattern.
	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE pending_invitations
			SET status = 'expired', kc_cleanup_pending = true
			WHERE id IN (
				SELECT id FROM pending_invitations
				WHERE status = 'pending' AND expires_at < now()
				ORDER BY expires_at
				LIMIT $1
			)
			RETURNING id, tenant_id`, jctx.BatchLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, tenantID string
			if err := rows.Scan(&id, &tenantID); err != nil {
				res.Failed++
				jctx.Logger.Warn("invitation-expiry scan failed", "error", err.Error())
				continue
			}
			res.Attempted++
			res.Succeeded++
		}
		return rows.Err()
	})
	if err != nil {
		return res, err
	}
	jctx.Logger.Info("invitation-expiry complete",
		"attempted", res.Attempted, "succeeded", res.Succeeded,
		"at", time.Now().UTC())
	return res, nil
}
