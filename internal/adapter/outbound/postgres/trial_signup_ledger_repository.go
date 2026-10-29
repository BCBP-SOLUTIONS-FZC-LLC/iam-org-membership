package postgres

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TrialSignupLedgerRepository operates on trial_signup_ledger (CR-4).
// The table has no RLS — it is cross-tenant by design (the eligibility check
// runs before the tenant row exists).
type TrialSignupLedgerRepository struct {
	pool *pgcommon.Pool
}

var _ port.TrialSignupLedgerRepository = (*TrialSignupLedgerRepository)(nil)

func NewTrialSignupLedgerRepository(pool *pgcommon.Pool) *TrialSignupLedgerRepository {
	return &TrialSignupLedgerRepository{pool: pool}
}

// ExistsEmailHash returns true when email_hash is already in the ledger.
func (r *TrialSignupLedgerRepository) ExistsEmailHash(ctx context.Context, emailHash string) (bool, error) {
	var exists bool
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM trial_signup_ledger WHERE email_hash = $1)`,
			emailHash)
		return row.Scan(&exists)
	})
	return exists, err
}

// ClaimEmailHash records email_hash for tenantID. The no-op DO UPDATE makes
// RETURNING yield the existing row on conflict (DO NOTHING returns nothing),
// and takes the row lock, so two concurrent I-1s for one hash serialize: the
// second sees the first's tenant_id once it commits.
func (r *TrialSignupLedgerRepository) ClaimEmailHash(ctx context.Context, emailHash string, tenantID uuid.UUID) error {
	var holder *uuid.UUID
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO trial_signup_ledger (email_hash, tenant_id) VALUES ($1, $2)
			 ON CONFLICT (email_hash) DO UPDATE SET tenant_id = trial_signup_ledger.tenant_id
			 RETURNING tenant_id`,
			emailHash, tenantID).Scan(&holder)
	})
	if err != nil {
		return err
	}
	if holder == nil || *holder != tenantID {
		return domain.NewError(domain.ErrTrialAlreadyUsed, "this email address has already been used for a trial")
	}
	return nil
}

// PruneCreatedBefore deletes up to limit ledger rows older than cutoff.
func (r *TrialSignupLedgerRepository) PruneCreatedBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	var n int64
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM trial_signup_ledger WHERE email_hash IN (
			     SELECT email_hash FROM trial_signup_ledger WHERE created_at < $1
			     ORDER BY created_at LIMIT $2)`,
			cutoff, limit)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}
