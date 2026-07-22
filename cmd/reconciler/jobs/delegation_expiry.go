package jobs

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DelegationExpiry sweeps active delegations past ends_at.
// Per §8.7 (DEL-6 pointer-clear semantics):
//  1. Call UP SetAvailability with {delegate_id: null} (never {status: available}
//     — UP owns the return-to-available transition).
//  2. On UP success, flip delegations.status='ended' + emit
//     DelegationEnded with ended_reason='expired'.
//  3. On UP failure, LEAVE the delegation active — next tick retries
//     (fail-open, DEL-6 defer).
func DelegationExpiry(ctx context.Context, jctx *Context) (Result, error) {
	var res Result

	type target struct {
		id          uuid.UUID
		tenantID    uuid.UUID
		delegatorID uuid.UUID
		delegateID  uuid.UUID
		scope       string
		scopeID     *uuid.UUID
		version     int64
	}
	var targets []target
	err := runInTxWithSysPool(ctx, jctx.SysPool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, delegator_id, delegate_id, scope, scope_id, record_version
			FROM delegations
			WHERE status = 'active' AND deleted_at IS NULL
			  AND ends_at IS NOT NULL AND ends_at < now()
			LIMIT $1`, jctx.BatchLimit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t target
			if err := rows.Scan(&t.id, &t.tenantID, &t.delegatorID, &t.delegateID, &t.scope, &t.scopeID, &t.version); err != nil {
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
		// Pointer-clear on UP FIRST.
		if err := jctx.UserProfile.SetAvailability(ctx, port.SetAvailabilityRequest{
			TenantID: t.tenantID, UserID: t.delegatorID, ClearDelegate: true,
		}); err != nil {
			jctx.Logger.Warn("delegation-expiry: UP pointer-clear failed — DEL-6 defer",
				"delegation_id", t.id, "error", err.Error())
			res.Failed++
			continue
		}
		// Local flip + event emission inside one tx.
		if err := endDelegationInTx(ctx, jctx, t); err != nil {
			jctx.Logger.Warn("delegation-expiry: local end failed",
				"delegation_id", t.id, "error", err.Error())
			res.Failed++
			continue
		}
		res.Succeeded++
	}
	jctx.Logger.Info("delegation-expiry complete",
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed,
		"at", time.Now().UTC())
	return res, nil
}

func endDelegationInTx(ctx context.Context, jctx *Context, t struct {
	id          uuid.UUID
	tenantID    uuid.UUID
	delegatorID uuid.UUID
	delegateID  uuid.UUID
	scope       string
	scopeID     *uuid.UUID
	version     int64
}) error {
	return jctx.TxRunner.RunInTx(ctx, func(txCtx context.Context) error {
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
			// Someone else raced (admin cancel, another tick) — skip emit.
			return nil
		}
		pub, _ := port.EventPublisherFromContext(txCtx)
		if pub == nil {
			return nil
		}
		return pub.EnqueueCtx(txCtx, &domain.DomainEvent{
			Type: domain.EventDelegationEnded, TenantID: t.tenantID,
			Subject: t.id.String(), Actor: "iam-system",
			Data: domain.DelegationEndedPayload{
				DelegationID: t.id, TenantID: t.tenantID,
				DelegatorID: t.delegatorID, DelegateID: t.delegateID,
				Scope: domain.DelegationScope(t.scope), ScopeID: t.scopeID,
				EndedReason: domain.EndReasonExpired,
			},
		})
	})
}
