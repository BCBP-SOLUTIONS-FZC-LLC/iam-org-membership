package jobs

import (
	"context"
	"time"
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
// Uses the BYPASSRLS InvitationRepository because the sweep spans tenants.
func InvitationExpiry(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	n, err := jctx.Invitations.ExpireOverdue(ctx, jctx.BatchLimit)
	if err != nil {
		return res, err
	}
	res.Attempted = n
	res.Succeeded = n
	jctx.Logger.Info("invitation-expiry complete",
		"attempted", res.Attempted, "succeeded", res.Succeeded,
		"at", time.Now().UTC())
	return res, nil
}
