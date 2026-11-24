-- EVT-14 recency watermark for the tenant's plan (LLD rev 2.62).
--
-- TenantPlanChanged (Billing) shared last_event_at with every other
-- lifecycle event. After a conversion reversal Billing's plan change is
-- stamped later than Realm Provisioner's TrialReactivated; delivered first,
-- it made TrialReactivated look stale, so the revert (realm reset, back to
-- trial) was skipped. Plan changes now use their own watermark, as
-- TenantSeatsChanged does (BL-OQ-2, seats_last_event_at). TenantConverted
-- and DirectPaidSignup, which also set plan, apply their plan only when
-- newer than this watermark and advance it when they do.
--
-- Numbered 000011, not 000001: 000001-000010 existed before they were
-- squashed into 000000 (#66). Reusing a number would let a database still
-- stamped with the old version skip this migration silently.
ALTER TABLE public.tenants ADD COLUMN plan_last_event_at timestamptz;

COMMENT ON COLUMN public.tenants.plan_last_event_at IS
    'EVT-14 recency high-water for the plan column: TenantPlanChanged, and the plan carried by TenantConverted/DirectPaidSignup — independent of last_event_at so plan changes cannot block lifecycle event processing';
