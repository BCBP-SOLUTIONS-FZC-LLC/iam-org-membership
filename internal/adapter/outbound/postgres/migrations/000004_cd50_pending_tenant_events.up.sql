-- 000004_cd50_pending_tenant_events — CD-50 park & replay (LLD rev 2.41, EVT-18).
--
-- iam.tenant.events and billing.events are unordered, so a lifecycle or
-- billing event can reach this service before the tenant row exists (e.g. a
-- direct-paid TenantConverted from RP-2 arriving ahead of the Signup BFF's
-- I-1). Instead of retrying until SQS gives up and DLQs the event (EVT-17),
-- the consumer parks it here and acknowledges it; I-1 replays a tenant's
-- parked events, in occurred_at order, as soon as it creates the row, and
-- the parked-events-replay CronJob backstops a replay that never ran.
--
-- No FK to tenants: the whole point is that the row does not exist yet.
-- Tenant-scoped like every other domain table (RLS on app.tenant_id), so
-- both the consumer and I-1 park/replay under the tenant's own GUC.
-- Additive only (MIG-1).

CREATE TABLE IF NOT EXISTS public.pending_tenant_events (
    event_id    uuid        NOT NULL,                  -- envelope id (same key as processed_events)
    tenant_id   uuid        NOT NULL,
    event_type  text        NOT NULL,
    occurred_at timestamptz NOT NULL,                  -- envelope time; replay order
    envelope    jsonb       NOT NULL,                  -- the envelope exactly as consumed
    parked_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pending_tenant_events_pkey PRIMARY KEY (event_id)
);

CREATE INDEX IF NOT EXISTS idx_pte_tenant_order ON public.pending_tenant_events (tenant_id, occurred_at, event_id);
CREATE INDEX IF NOT EXISTS idx_pte_parked_at    ON public.pending_tenant_events (parked_at);

ALTER TABLE public.pending_tenant_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.pending_tenant_events FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON public.pending_tenant_events;
CREATE POLICY tenant_isolation ON public.pending_tenant_events
    USING      (rls_check_tenant(tenant_id, 'pending_tenant_events'))
    WITH CHECK (rls_check_tenant(tenant_id, 'pending_tenant_events'));

REVOKE ALL ON public.pending_tenant_events FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
        BEGIN
            -- Park (INSERT), list for replay (SELECT), claim on replay (DELETE).
            GRANT SELECT, INSERT, DELETE ON public.pending_tenant_events TO org_membership_app;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'Grant to org_membership_app skipped — current user (%) lacks GRANT privilege.', current_user;
        END;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        BEGIN
            GRANT SELECT ON public.pending_tenant_events TO admin_readonly;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'Grant to admin_readonly skipped — current user (%) lacks GRANT privilege.', current_user;
        END;
    END IF;
END$$;
