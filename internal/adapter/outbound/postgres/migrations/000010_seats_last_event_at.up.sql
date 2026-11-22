-- 000010_seats_last_event_at — BL-OQ-2: separate EVT-14 recency watermark
-- for seat-change events so TenantSeatsChanged cannot block lifecycle events.
--
-- Problem: tenants.last_event_at is a single high-water mark bumped by every
-- event type. If a Billing TenantSeatsChanged arrives and advances last_event_at
-- to T1 before an in-flight TenantConverted (with timestamp T0 < T1) is
-- processed, the conversion is skipped by EVT-14 (event.time <= last_event_at)
-- even though no lifecycle projection has run at T1.
--
-- Fix: give licensed_seats its own recency column. TenantSeatsChanged now
-- checks seats_last_event_at and bumps only that column; all lifecycle events
-- continue to use last_event_at exclusively. The two watermarks are
-- independent, so a seat change can never advance the lifecycle guard.
--
-- NULL at startup is correct — the first event of each type always applies.
-- Additive only (MIG-1).

ALTER TABLE public.tenants
    ADD COLUMN seats_last_event_at timestamptz;

COMMENT ON COLUMN public.tenants.seats_last_event_at IS
    'EVT-14 recency high-water for TenantSeatsChanged — independent of last_event_at so seat changes cannot block lifecycle event processing (BL-OQ-2)';
