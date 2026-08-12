package jobs

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	reviewWarnEarlyDays = 7
)

type delegationReviewTarget struct {
	id          uuid.UUID
	tenantID    uuid.UUID
	delegatorID uuid.UUID
	delegateID  uuid.UUID
	scope       string
	scopeID     *uuid.UUID
	reviewDueAt time.Time
	version     int64
}

type delegationEndTarget struct {
	id          uuid.UUID
	tenantID    uuid.UUID
	delegatorID uuid.UUID
	delegateID  uuid.UUID
	scope       string
	scopeID     *uuid.UUID
	version     int64
}

// DelegationReview sweeps open-ended delegations (ends_at IS NULL) that have
// a review_due_at set. It performs two passes per tick:
//  1. Warn: emit DelegationReviewRequested for delegations within the warning
//     window (7 d before review_due_at) that have not yet been warned.
//  2. Auto-end: for delegations past review_due_at, end them via the existing
//     DelegationEnded mechanism (ended_reason="review_expired", §16 A70 / DEL-13).
//
// CronJob sentinel: ip_address="system", user_agent="iam-org-membership/delegation-review-cron".
func DelegationReview(ctx context.Context, jctx *Context) (Result, error) {
	var res Result
	now := time.Now().UTC()

	// Pass 1: warn — delegations whose review_due_at is within the early
	// warning window and have not yet been warned (review_notice_sent_at IS NULL).
	earlyWarn := now.Add(time.Duration(reviewWarnEarlyDays) * 24 * time.Hour)

	var warnTargets []delegationReviewTarget
	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, delegator_id, delegate_id, scope, scope_id, review_due_at, record_version
			FROM delegations
			WHERE ends_at IS NULL AND status = 'active' AND deleted_at IS NULL
			  AND review_due_at IS NOT NULL AND review_due_at > $1
			  AND review_due_at <= $2 AND review_notice_sent_at IS NULL
			LIMIT $3`, now, earlyWarn, jctx.BatchLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t delegationReviewTarget
			if err := rows.Scan(&t.id, &t.tenantID, &t.delegatorID, &t.delegateID, &t.scope, &t.scopeID, &t.reviewDueAt, &t.version); err != nil {
				return err
			}
			warnTargets = append(warnTargets, t)
		}
		return rows.Err()
	})
	if err != nil {
		return res, err
	}

	for _, t := range warnTargets {
		res.Attempted++
		if err := warnReviewDue(ctx, jctx, t, now); err != nil {
			jctx.Logger.Warn("delegation-review: warn failed",
				"delegation_id", t.id, "error", err.Error())
			res.Failed++
			continue
		}
		res.Succeeded++
	}

	// Pass 2: auto-end — delegations whose review_due_at has passed.
	var endTargets []delegationEndTarget
	err = runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, delegator_id, delegate_id, scope, scope_id, record_version
			FROM delegations
			WHERE ends_at IS NULL AND status = 'active' AND deleted_at IS NULL
			  AND review_due_at IS NOT NULL AND review_due_at <= $1
			LIMIT $2`, now, jctx.BatchLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t delegationEndTarget
			if err := rows.Scan(&t.id, &t.tenantID, &t.delegatorID, &t.delegateID, &t.scope, &t.scopeID, &t.version); err != nil {
				return err
			}
			endTargets = append(endTargets, t)
		}
		return rows.Err()
	})
	if err != nil {
		return res, err
	}

	for _, t := range endTargets {
		res.Attempted++
		// Pointer-clear on UP FIRST (same as delegation_expiry DEL-6).
		if err := jctx.UserProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
			TenantID: t.tenantID, UserID: t.delegatorID, ClearDelegate: true,
		}); err != nil {
			jctx.Logger.Warn("delegation-review: UP pointer-clear failed — DEL-6 defer",
				"delegation_id", t.id, "error", err.Error())
			res.Failed++
			continue
		}
		if err := endReviewExpiredInTx(ctx, jctx, t); err != nil {
			jctx.Logger.Warn("delegation-review: auto-end failed",
				"delegation_id", t.id, "error", err.Error())
			res.Failed++
			continue
		}
		res.Succeeded++
	}

	jctx.Logger.Info("delegation-review complete",
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed,
		"at", now)
	return res, nil
}

func warnReviewDue(ctx context.Context, jctx *Context, t delegationReviewTarget, now time.Time) error {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = t.tenantID.String()
	gucCtx := pgcommon.WithGUCSet(ctx, g)
	return jctx.TxRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
		tx, ok := txFromCtx(txCtx)
		if !ok {
			return errNoTx
		}
		// Mark notice sent atomically.
		cmd, err := tx.Exec(txCtx, `
			UPDATE delegations SET review_notice_sent_at = $3
			WHERE id = $1 AND record_version = $2 AND status = 'active' AND deleted_at IS NULL`,
			t.id, t.version, now)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return nil // raced — skip
		}
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub == nil {
			return nil
		}
		return pub.EnqueueCtx(txCtx, &domain.DomainEvent{
			Type:      domain.EventDelegationReviewRequested,
			TenantID:  t.tenantID,
			Subject:   t.id.String(),
			Actor:     "iam-system",
			IPAddress: "system",
			UserAgent: "iam-org-membership/delegation-review-cron",
			Data: domain.DelegationReviewRequestedPayload{
				DelegationID: t.id, TenantID: t.tenantID,
				DelegatorID: t.delegatorID, DelegateID: t.delegateID,
				Scope: domain.DelegationScope(t.scope), ScopeID: t.scopeID,
				ReviewDueAt: t.reviewDueAt,
				ActorID:     domain.SystemActorID,
			},
		})
	})
}

func endReviewExpiredInTx(ctx context.Context, jctx *Context, t delegationEndTarget) error {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = "iam-system"
	g.TenantID = t.tenantID.String()
	gucCtx := pgcommon.WithGUCSet(ctx, g)
	return jctx.TxRunner.RunInTx(gucCtx, func(txCtx context.Context) error {
		tx, ok := txFromCtx(txCtx)
		if !ok {
			return errNoTx
		}
		cmd, err := tx.Exec(txCtx, `
			UPDATE delegations SET status = 'ended'
			WHERE id = $1 AND record_version = $2 AND status = 'active' AND deleted_at IS NULL`,
			t.id, t.version)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return nil // raced — skip
		}
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub == nil {
			return nil
		}
		return pub.EnqueueCtx(txCtx, &domain.DomainEvent{
			Type:      domain.EventDelegationEnded,
			TenantID:  t.tenantID,
			Subject:   t.id.String(),
			Actor:     "iam-system",
			IPAddress: "system",
			UserAgent: "iam-org-membership/delegation-review-cron",
			Data: domain.DelegationEndedPayload{
				DelegationID: t.id, TenantID: t.tenantID,
				DelegatorID: t.delegatorID, DelegateID: t.delegateID,
				Scope: domain.DelegationScope(t.scope), ScopeID: t.scopeID,
				EndedReason: domain.EndReasonReviewExpired,
				ActorID:     domain.SystemActorID,
			},
		})
	})
}
