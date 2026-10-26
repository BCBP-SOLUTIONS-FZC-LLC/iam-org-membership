package postgres

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
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

// InsertEmailHash inserts email_hash into the ledger. ON CONFLICT DO NOTHING
// makes repeated calls for the same hash a safe no-op (idempotent I-1 replay).
func (r *TrialSignupLedgerRepository) InsertEmailHash(ctx context.Context, emailHash string) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO trial_signup_ledger (email_hash) VALUES ($1) ON CONFLICT DO NOTHING`,
			emailHash)
		return err
	})
}
