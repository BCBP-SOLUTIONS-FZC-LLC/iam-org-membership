package port

import "context"

// TrialSignupLedgerRepository is the CR-4 port for the trial_signup_ledger
// table. The table records every email address (as an HMAC-SHA256 hex hash)
// that has been used for a trial signup, so the BFF can gate double-trial
// attempts before calling I-1. It has no tenant scope and no RLS.
type TrialSignupLedgerRepository interface {
	// ExistsEmailHash returns true when email_hash already has a row in
	// trial_signup_ledger. Used by CheckTrialEligibility (CR-4).
	ExistsEmailHash(ctx context.Context, emailHash string) (bool, error)

	// InsertEmailHash inserts email_hash into trial_signup_ledger.
	// ON CONFLICT DO NOTHING — idempotent: a re-run of I-1 on the same hash
	// (TrialSignup idempotent replay) does not fail.
	InsertEmailHash(ctx context.Context, emailHash string) error
}
