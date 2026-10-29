-- Rollback for 000006_cr4_trial_ledger_hardening. Grants are left in place
-- (harmless, and 000005's down drops the table anyway).
DROP INDEX IF EXISTS public.idx_tsl_created_at;
ALTER TABLE public.trial_signup_ledger DROP CONSTRAINT IF EXISTS trial_signup_ledger_email_hash_format;
ALTER TABLE public.trial_signup_ledger DROP COLUMN IF EXISTS tenant_id;
