-- The plan a tenant had during its trial, recorded at conversion so an RP-4
-- conversion revert can restore it (LLD rev 2.65; Billing's answer to the
-- revert plan question, 2026-11-25).
--
-- Set by the TenantConverted/DirectPaidSignup projection from the row's plan
-- when the tenant converts from 'trial' or 'trial_expired'; read by the
-- TrialReactivated(conversion_revert) projection, which restores it. NULL
-- means no trial plan was recorded: the revert keeps the current plan and
-- logs a warning rather than inventing one.
ALTER TABLE public.tenants ADD COLUMN trial_plan public.tenant_plan;

COMMENT ON COLUMN public.tenants.trial_plan IS
    'Plan during the trial, recorded at conversion; restored by an RP-4 conversion revert. NULL = not recorded (the revert keeps the current plan)';
