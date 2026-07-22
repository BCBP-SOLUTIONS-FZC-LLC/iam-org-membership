package jobs

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	type candidate struct {
		tenantID uuid.UUID
	}
	var candidates []candidate
	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id FROM tenants
			WHERE deleted_at IS NULL
			  AND (overage_since IS NOT NULL OR licensed_seats > 0)
			LIMIT $1`, jctx.BatchLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.tenantID); err != nil {
				return err
			}
			candidates = append(candidates, c)
		}
		return rows.Err()
	})
	if err != nil {
		return res, err
	}

	for _, c := range candidates {
		res.Attempted++
		if err := reconcileOneTenant(ctx, jctx, c.tenantID); err != nil {
			jctx.Logger.Warn("seat-overage-reconcile: tenant failed",
				"tenant_id", c.tenantID, "error", err.Error())
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
	// Use the txRunner so any emitted event goes into the same tx as the
	// tenants UPDATE.
	return jctx.TxRunner.RunInTx(ctx, func(txCtx context.Context) error {
		tx, ok := txFromCtx(txCtx)
		if !ok {
			return errNoTx
		}
		var licensedSeats int
		var overageSince *time.Time
		if err := tx.QueryRow(txCtx, `
			SELECT licensed_seats, overage_since FROM tenants
			WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, tenantID).
			Scan(&licensedSeats, &overageSince); err != nil {
			return err
		}
		var active, pending int
		if err := tx.QueryRow(txCtx, `
			SELECT count(*) FROM tenant_memberships
			WHERE tenant_id = $1 AND deleted_at IS NULL AND status = 'active'`, tenantID).Scan(&active); err != nil {
			return err
		}
		if err := tx.QueryRow(txCtx, `
			SELECT count(*) FROM pending_invitations
			WHERE tenant_id = $1 AND status = 'pending' AND expires_at > now()`, tenantID).Scan(&pending); err != nil {
			return err
		}
		overCap := active+pending > licensedSeats

		pub, _ := port.EventPublisherFromContext(txCtx)

		switch {
		case overCap && overageSince == nil:
			now := time.Now().UTC()
			if _, err := tx.Exec(txCtx, `UPDATE tenants SET overage_since = $2 WHERE id = $1`, tenantID, now); err != nil {
				return err
			}
			if pub != nil {
				_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
					Type: domain.EventTenantSeatOverageStarted, TenantID: tenantID,
					Subject: tenantID.String(), Actor: "iam-system",
					Data: domain.TenantSeatOverageStartedPayload{
						TenantID: tenantID, LicensedSeats: licensedSeats,
						ActiveUsers: active, PendingInvitations: pending, OverageSince: now,
					},
				})
			}
		case !overCap && overageSince != nil:
			now := time.Now().UTC()
			if _, err := tx.Exec(txCtx, `UPDATE tenants SET overage_since = NULL WHERE id = $1`, tenantID); err != nil {
				return err
			}
			if pub != nil {
				_ = pub.EnqueueCtx(txCtx, &domain.DomainEvent{
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
