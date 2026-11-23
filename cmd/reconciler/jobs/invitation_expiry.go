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

	// SECURITY-8 deferred erase for revokes: P-31 keeps the email while the
	// PI-11 cooldown can still match it; once the window has passed it is
	// scrubbed here. A failure fails the run (after the expiry above has
	// committed) so the CronJob-failure alert fires: this sweep is the only
	// thing that erases a revoked invitee's PII, so it must never fail quietly.
	erased, eerr := jctx.Invitations.EraseRevokedPII(ctx, time.Now().UTC().Add(-jctx.ReinviteCooldown), jctx.BatchLimit)
	if eerr != nil {
		res.Failed++
		return res, fmt.Errorf("invitation-expiry: erase revoked-invitation PII: %w", eerr)
	}
	jctx.Logger.Info("invitation-expiry complete",
		"attempted", res.Attempted, "succeeded", res.Succeeded,
		"revoked_pii_erased", erased,
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
