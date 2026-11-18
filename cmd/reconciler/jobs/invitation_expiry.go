package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
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
// One invitation.expired audit entry per expired invitation is enqueued in
// the same transaction as the UPDATE (PA-OQ-20), so an entry exists if and
// only if the expiry committed; the server's audit runner delivers it.
//
// Uses the BYPASSRLS InvitationRepository and SysTxRunner because the sweep
// spans tenants.
func InvitationExpiry(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	var expired []domain.ExpiredInvitation
	sweep := func(txCtx context.Context) error {
		rows, err := jctx.Invitations.ExpireOverdue(txCtx, jctx.BatchLimit)
		if err != nil {
			return err
		}
		expired = rows
		return recordExpired(txCtx, rows)
	}
	var err error
	if jctx.SysTxRunner != nil {
		err = jctx.SysTxRunner.RunInTx(ctx, sweep)
	} else {
		err = sweep(ctx)
	}
	if err != nil {
		return res, err
	}
	res.Attempted = len(expired)
	res.Succeeded = len(expired)

	// PI-11: erase PII from revoked invitations whose cooldown window has
	// closed. The cooldown is counted from created_at so a re-invite can be
	// refused during the window; once it passes the email is no longer needed.
	if jctx.ReinviteCooldown > 0 {
		cutoff := time.Now().Add(-jctx.ReinviteCooldown)
		erased, eraseErr := jctx.Invitations.EraseRevokedPII(ctx, cutoff, jctx.BatchLimit)
		if eraseErr != nil {
			jctx.Logger.Warn("invitation-expiry: EraseRevokedPII failed (non-fatal)",
				"error", eraseErr)
		} else {
			jctx.Logger.Info("invitation-expiry: revoked PII erased", "count", erased)
		}
	}

	jctx.Logger.Info("invitation-expiry complete",
		"attempted", res.Attempted, "succeeded", res.Succeeded,
		"at", time.Now().UTC())
	return res, nil
}

// recordExpired enqueues one invitation.expired entry per row: system actor,
// the invitation as target, OccurredAt = expires_at (the business time, the
// same on every retry of the sweep). No recorder in ctx skips it.
func recordExpired(ctx context.Context, rows []domain.ExpiredInvitation) error {
	rec, ok := port.AuditRecorderFromContext(ctx)
	if !ok || len(rows) == 0 {
		return nil
	}
	entries := make([]port.AuditEntry, 0, len(rows))
	for _, r := range rows {
		entries = append(entries, port.AuditEntry{
			IdempotencyKey:  fmt.Sprintf("%s:%s:%s", port.AuditEntryInvitationExpired, r.TenantID, r.ID),
			TenantID:        r.TenantID,
			EntryType:       port.AuditEntryInvitationExpired,
			Action:          "expire",
			Actor:           port.SystemAuditActor(),
			Target:          &port.AuditTarget{Type: "invitation", ID: r.ID.String()},
			OccurredAt:      r.ExpiresAt,
			SourceEventType: "InvitationExpired",
			UserAgent:       cronUserAgent("invitation-expiry"),
		})
	}
	return rec.RecordBatch(ctx, entries)
}
