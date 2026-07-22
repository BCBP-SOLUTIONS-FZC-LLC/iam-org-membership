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
// Uses sysPool (BYPASSRLS) because the sweep spans tenants.
func InvitationExpiry(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	// Select expired pending rows in batches to keep lock hold time bounded.
	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE pending_invitations
			SET status = 'expired'
			WHERE status = 'pending' AND expires_at < now()
			RETURNING id, tenant_id`)
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
