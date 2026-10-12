package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
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

	// AL-5 best-effort audit (invitation.expired, D-6). Emit one batch entry
	// per reconciler run when at least one invitation was expired. Uses a
	// per-minute idempotency key so back-to-back retries within the same
	// minute produce a single dedup-collapsed entry in the Audit Log.
	if n > 0 && jctx.AuditLog != nil {
		auditInvitationExpiredBatch(ctx, jctx.AuditLog, n)
	}

	return res, nil
}

// auditInvitationExpiredBatch posts a single invitation.expired entry
// representing the full sweep batch (fail-open, 5s hard cap).
func auditInvitationExpiredBatch(_ context.Context, client port.AuditLogClient, count int) {
	idemKey := fmt.Sprintf("invitation.expired:batch:%s", time.Now().UTC().Format("2006-01-02T15:04"))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.WriteEntry(ctx, port.AuditWriteRequest{
			IdempotencyKey:  idemKey,
			TenantID:        uuid.Nil, // cross-tenant sweep; Audit Log accepts uuid.Nil for system-scope
			EntryType:       "invitation.expired",
			Action:          "expire",
			Actor:           port.AuditActor{Type: "iam_system", ID: "iam-system"},
			OccurredAt:      time.Now().UTC(),
			SourceService:   "iam-org-membership",
			SourceEventType: "InvitationExpiredBatch",
			IPAddress:       cronIPAddress,
			UserAgent:       cronUserAgent("invitation-expiry"),
			Metadata:        map[string]any{"count": count},
		}); err != nil {
			_ = err // fail-open — audit outage must not fail the CronJob
		}
	}()
}
