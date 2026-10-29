package port

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// TrialSignupLedgerRepository is the CR-4 port for the trial_signup_ledger
// table. The table records every email address (as an HMAC-SHA256 hex hash)
// that has been used for a trial signup, and which tenant claimed it, so the
// BFF can gate double-trial attempts before calling I-1 and I-1 itself can
// refuse a second trial. It has no tenant scope and no RLS.
type TrialSignupLedgerRepository interface {
	// ExistsEmailHash returns true when email_hash already has a row in
	// trial_signup_ledger. Used by CheckTrialEligibility (CR-4).
	ExistsEmailHash(ctx context.Context, emailHash string) (bool, error)

	// ClaimEmailHash records email_hash for tenantID inside the caller's
	// transaction. A hash already held by tenantID is a no-op (I-1
	// replay); one held by any other tenant (or by a row written before
	// tenant_id existed) returns domain.ErrTrialAlreadyUsed, so the caller's
	// I-1 transaction rolls back. Concurrent claims of one hash serialize on
	// its primary key: the loser sees the winner's tenant.
	ClaimEmailHash(ctx context.Context, emailHash string, tenantID uuid.UUID) error

	// PruneCreatedBefore deletes up to limit rows created before cutoff and
	// returns how many it deleted (trial-ledger-prune retention job).
	PruneCreatedBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}
