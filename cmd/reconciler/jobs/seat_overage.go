package jobs

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
)

// SeatOverageReconcile is the SEAT-5 backstop cron. Every tenant with a
// non-null overage_since gets its actual (active + pending) counts
// recomputed under FOR UPDATE. Transitions:
//
//	overage_since IS NOT NULL && active + pending <= licensed_seats
//	    → clear overage_since; emit TenantSeatOverageResolved
//	overage_since IS NULL && active + pending > licensed_seats
//	    → set overage_since=now(); emit TenantSeatOverageStarted
//
// Regular SEAT-1 mutators (invite create, member add/remove, seats change)
// also handle these transitions inline; this job is the daily backstop
// for edge cases (e.g. TenantSeatsChanged reducing licensed_seats through
// the SQS consumer). Both transitions emit their event atomically with
// the tenants UPDATE (EVT-10).
func SeatOverageReconcile(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	// Sweep candidates: tenants with overage_since set OR tenants that
	// might be newly over-cap. Cheap over-approximation — cross-check
	// under FOR UPDATE per row.
	var candidates []uuid.UUID
	candidates, err := jctx.Reconciler.ListSeatOverageCandidates(ctx, jctx.BatchLimit)
	if err != nil {
		return res, err
	}

	for _, tenantID := range candidates {
		res.Attempted++
		if err := reconcileOneTenant(ctx, jctx, tenantID); err != nil {
			jctx.Logger.Warn("seat-overage-reconcile: tenant failed",
				"tenant_id", tenantID, "error", err.Error())
			res.Failed++
			continue
		}
		res.Succeeded++
	}
	jctx.Logger.Info("seat-overage-reconcile complete",
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed)
	return res, nil
}

func reconcileOneTenant(ctx context.Context, jctx *Context, tenantID uuid.UUID) error {
	// RLS-6 (LLD line 1746): the TxRunner runs against the RLS-scoped app pool
	// (GUCProvider=pgcommon.GUCSetFromContext), so app.tenant_id must be bound
	// on this ctx or every UPDATE fails the tenant_isolation WITH CHECK and
	// returns 0 rows silently — defeating SEAT-5's overage_since transitions.
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = tenantID.String()
	gucCtx := pgcommon.WithGUCSet(ctx, g)
	// Use the txRunner so any emitted event goes into the same tx as the
	// tenants UPDATE.
	return jctx.TxRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
		occ, err := jctx.Tenants.LockSeatOccupancy(txCtx, tenantID)
		if err != nil {
			return err
		}
		overCap := occ.Active+occ.Pending > occ.LicensedSeats

		pub, _ := port.EventPublisherFromContext(txCtx)

		switch {
		case overCap && occ.OverageSince == nil:
			now := time.Now().UTC()
			if err := jctx.Tenants.SetOverageSince(txCtx, tenantID, &now); err != nil {
				return err
			}
			if pub != nil {
				_ = pub.Enqueue(txCtx, &domain.DomainEvent{
					Type: domain.EventTenantSeatOverageStarted, TenantID: tenantID,
					Subject: tenantID.String(), Actor: "iam-system",
					Data: domain.TenantSeatOverageStartedPayload{
						TenantID: tenantID, LicensedSeats: occ.LicensedSeats,
						ActiveUsers: occ.Active, PendingInvitations: occ.Pending, OverageSince: now,
					},
				})
			}
		case !overCap && occ.OverageSince != nil:
			now := time.Now().UTC()
			if err := jctx.Tenants.SetOverageSince(txCtx, tenantID, nil); err != nil {
				return err
			}
			if pub != nil {
				_ = pub.Enqueue(txCtx, &domain.DomainEvent{
					Type: domain.EventTenantSeatOverageResolved, TenantID: tenantID,
					Subject: tenantID.String(), Actor: "iam-system",
					Data: domain.TenantSeatOverageResolvedPayload{TenantID: tenantID, ResolvedAt: now},
				})
			}
		default:
			// No transition — either both true (still over-cap, keep
			// overage_since) or both false (still under cap, keep NULL).
		}
		return nil
	})
}
