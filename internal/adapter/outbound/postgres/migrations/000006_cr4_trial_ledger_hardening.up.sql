-- 000006_cr4_trial_ledger_hardening — CR-4 trial_signup_ledger fixes.
--
-- 1. Grants. 000005 granted nothing, so in staging/prod (where
--    org_membership_app is a distinct low-privilege role, not the owner)
--    every CR-4 check and every BFF I-1 carrying email_hash failed with
--    permission denied. The app role gets SELECT (CR-4), INSERT (I-1) and
--    DELETE (trial-ledger-prune); PUBLIC gets nothing.
-- 2. tenant_id: which tenant's I-1 claimed the hash. I-1 now fails with
--    TRIAL_ALREADY_USED when a different tenant holds it, instead of
--    silently skipping the insert, which closes the CR-4 → I-1
--    check-then-act race. NULL only for rows written before this
--    migration; such a row counts as held by another tenant.
-- 3. Format: the hash is exactly 64 lower-case hex characters
--    (HMAC-SHA256). NOT VALID: checked for new rows only.
-- 4. created_at index for the trial-ledger-prune retention job.
-- Additive only (MIG-1).

ALTER TABLE public.trial_signup_ledger ADD COLUMN IF NOT EXISTS tenant_id uuid;

ALTER TABLE public.trial_signup_ledger DROP CONSTRAINT IF EXISTS trial_signup_ledger_email_hash_format;
ALTER TABLE public.trial_signup_ledger
    ADD CONSTRAINT trial_signup_ledger_email_hash_format
    CHECK (email_hash ~ '^[0-9a-f]{64}$') NOT VALID;

CREATE INDEX IF NOT EXISTS idx_tsl_created_at ON public.trial_signup_ledger (created_at);

REVOKE ALL ON public.trial_signup_ledger FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
        BEGIN
            -- CR-4 check (SELECT), I-1 claim (INSERT ... ON CONFLICT DO
            -- UPDATE needs UPDATE too), retention prune (DELETE).
            GRANT SELECT, INSERT, UPDATE, DELETE ON public.trial_signup_ledger TO org_membership_app;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'Grant to org_membership_app skipped — current user (%) lacks GRANT privilege.', current_user;
        END;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        BEGIN
            GRANT SELECT ON public.trial_signup_ledger TO admin_readonly;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'Grant to admin_readonly skipped — current user (%) lacks GRANT privilege.', current_user;
        END;
    END IF;
END$$;
