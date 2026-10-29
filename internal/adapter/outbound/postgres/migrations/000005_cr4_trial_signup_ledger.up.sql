-- CR-4: trial_signup_ledger — cross-tenant email-hash ledger that records
-- every email address (as an HMAC-SHA256 hex hash) that has been used for a
-- trial tenant signup. Written atomically inside the I-1 TrialSignup
-- transaction; checked by POST /api/v1/internal/trial-eligibility (CR-4).
--
-- No RLS: this table is pre-tenant by design (the eligibility check runs
-- before the tenant row exists) and the hash is already irreversible, so
-- per-tenant isolation would add no security benefit.
CREATE TABLE trial_signup_ledger (
    email_hash  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT trial_signup_ledger_pkey
        PRIMARY KEY (email_hash),

    -- HMAC-SHA256 hex is exactly 64 chars; allow 1-128 for forward compat.
    CONSTRAINT trial_signup_ledger_email_hash_length
        CHECK (char_length(email_hash) BETWEEN 1 AND 128)
);
