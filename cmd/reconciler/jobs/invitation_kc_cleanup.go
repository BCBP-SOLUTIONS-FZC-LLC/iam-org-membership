package jobs

import (
	"context"
)

// InvitationKCCleanup sweeps pending_invitations with kc_cleanup_pending=true
// (PI-9 durable-marker reconciler). For each row:
//  1. Call RP DeleteUser (idempotent; 404 treated as success).
//  2. Clear kc_cleanup_pending on 200.
//  3. Leave the marker set on any RP failure — next tick retries.
//
// Runs against the BYPASSRLS InvitationRepository since sweeps cross tenants.
func InvitationKCCleanup(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	targets, err := jctx.Invitations.ListPendingKCCleanup(ctx, jctx.BatchLimit)
	if err != nil {
		return res, err
	}

	for _, t := range targets {
		res.Attempted++
		if t.KeycloakUserID == nil {
			// Nothing to delete in Keycloak; clear the marker to avoid a
			// stuck row.
			if cerr := jctx.Invitations.ClearKCCleanupPendingByID(ctx, t.ID); cerr != nil {
				res.Failed++
				continue
			}
			res.Skipped++
			continue
		}
		if err := jctx.RealmProvisioner.DeleteUser(ctx, t.TenantID, *t.KeycloakUserID); err != nil {
			// Leave the marker set; next tick retries. Log at Warn (not Error)
			// because DEL-6 fail-open is the design.
			jctx.Logger.Warn("invitation-kc-cleanup: RP DeleteUser failed — leaving marker",
				"invitation_id", t.ID, "keycloak_user_id", *t.KeycloakUserID, "error", err.Error())
			res.Failed++
			continue
		}
		if err := jctx.Invitations.ClearKCCleanupPendingByID(ctx, t.ID); err != nil {
			jctx.Logger.Warn("invitation-kc-cleanup: clear marker failed", "invitation_id", t.ID, "error", err.Error())
			res.Failed++
			continue
		}
		res.Succeeded++
	}
	jctx.Logger.Info("invitation-kc-cleanup complete",
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed, "skipped", res.Skipped)
	return res, nil
}
