package jobs

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
)

// TrialCleanup hard-deletes trial_expired tenants past the grace window
// (§8.10.3, default 15 days). Trial lifecycle terminates here — paid
// tenants never hard-delete (PAID-1 terminal at 'offboarded' with
// soft-delete + PII scrub).
//
// Row selection: status='trial_expired' AND trial_ends_at < now() - grace.
// Child rows cascade via ON DELETE CASCADE (§4.2). trial_signup_ledger is
// retained (TRIAL-2 one-lifetime-trial).
//
// Each tenant is deleted in its own TxRunner transaction so the
// TenantMembershipsPurged outbox entry is atomic with the DELETE. This
// mirrors the paid-offboarding path in membership_event_consumer.go and
// gives Delegation, Tender-ACL, and Group-Mapping the cascade signal they
// need to clean up their own tenant-scoped rows (ADR-0008 §6.4).
func TrialCleanup(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	candidates, err := jctx.Reconciler.ListExpiredTrialTenants(ctx, jctx.TrialGraceDays, jctx.BatchLimit)
	if err != nil {
		return res, err
	}

	for _, tenantID := range candidates {
		res.Attempted++
		if err := deleteOneTrial(ctx, jctx, tenantID); err != nil {
			jctx.Logger.Warn("trial-cleanup: tenant failed",
				"tenant_id", tenantID, "error", err.Error())
			res.Failed++
			continue
		}
		res.Succeeded++
	}
	jctx.Logger.Info("trial-cleanup complete",
		"deleted", res.Succeeded, "grace_days", jctx.TrialGraceDays)
	return res, nil
}

func deleteOneTrial(ctx context.Context, jctx *Context, tenantID uuid.UUID) error {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = tenantID.String()
	gucCtx := pgcommon.WithGUCSet(ctx, g)
	return jctx.TxRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
		if err := jctx.Tenants.HardDeleteTenant(txCtx, tenantID); err != nil {
			return err
		}
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub == nil {
			return nil
		}
		return pub.Enqueue(txCtx, &domain.DomainEvent{
			Type:      domain.EventTenantMembershipsPurged,
			TenantID:  tenantID,
			Subject:   tenantID.String(),
			Actor:     "iam-system",
			IPAddress: cronIPAddress,
			UserAgent: cronUserAgent("trial-cleanup"),
			Data: domain.TenantMembershipsPurgedPayload{
				TenantID: tenantID,
				ActorID:  domain.SystemActorID,
			},
		})
	})
}
