package jobs

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// InvitationKCCleanup sweeps pending_invitations with kc_cleanup_pending=true
// (PI-9 durable-marker reconciler). For each row:
//  1. Call RP DeleteUser (idempotent; 404 treated as success).
//  2. Clear kc_cleanup_pending on 200.
//  3. Leave the marker set on any RP failure — next tick retries.
//
// Runs on sysPool (BYPASSRLS) since sweeps cross tenants.
func InvitationKCCleanup(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	type target struct {
		id             uuid.UUID
		tenantID       uuid.UUID
		keycloakUserID *uuid.UUID
	}
	var targets []target

	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, keycloak_user_id FROM pending_invitations
			WHERE kc_cleanup_pending = true
			LIMIT $1`, jctx.BatchLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t target
			if err := rows.Scan(&t.id, &t.tenantID, &t.keycloakUserID); err != nil {
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
		if t.keycloakUserID == nil {
			// Nothing to delete in Keycloak; clear the marker to avoid a
			// stuck row.
			if cerr := clearKCCleanupPending(ctx, jctx, t.id); cerr != nil {
				res.Failed++
				continue
			}
			res.Skipped++
			continue
		}
		if err := jctx.RealmProvisioner.DeleteUser(ctx, t.tenantID, *t.keycloakUserID); err != nil {
			// Leave the marker set; next tick retries. Log at Warn (not Error)
			// because DEL-6 fail-open is the design.
			jctx.Logger.Warn("invitation-kc-cleanup: RP DeleteUser failed — leaving marker",
				"invitation_id", t.id, "keycloak_user_id", *t.keycloakUserID, "error", err.Error())
			res.Failed++
			continue
		}
		if err := clearKCCleanupPending(ctx, jctx, t.id); err != nil {
			jctx.Logger.Warn("invitation-kc-cleanup: clear marker failed", "invitation_id", t.id, "error", err.Error())
			res.Failed++
			continue
		}
		res.Succeeded++
	}
	jctx.Logger.Info("invitation-kc-cleanup complete",
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed, "skipped", res.Skipped)
	return res, nil
}

func clearKCCleanupPending(ctx context.Context, jctx *Context, id uuid.UUID) error {
	return runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE pending_invitations SET kc_cleanup_pending = false WHERE id = $1`, id)
		return err
	})
}

// suppress unused-import warning when Phase 6 removes RP client usage
var _ = port.RealmProvisionerClient(nil)
