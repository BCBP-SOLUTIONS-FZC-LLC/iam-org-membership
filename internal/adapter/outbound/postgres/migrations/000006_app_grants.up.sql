-- Phase 1 · Migration 000006_app_grants — LLD §4.3 / RLS-4 / MIG-3
--
-- Grants the runtime `org_membership_app` role the table-level privileges it
-- needs to reach the RLS policy layer. RLS-4 requires this role to be
-- RLS-scoped (no BYPASSRLS), but a policy can only apply to a role that
-- already has table-level DML — otherwise every SELECT/INSERT fails at the
-- privilege check before RLS is consulted.
--
-- In local dev this is masked because `POSTGRES_USER=org_membership_app`
-- starts as the DB owner (init-db.sql), so grants are implicit. In staging
-- and production `org_membership_app` is a distinct low-privilege role
-- provisioned by Terraform (init-db.sql lines 32-34 promised these grants
-- would land here); without this migration every query fails permission
-- denied at first cutover.
--
-- Grant matrix:
--   • 12 tenant-scoped tables — SELECT / INSERT / UPDATE / DELETE
--     (writes still gated by the tenant_isolation RLS policy, RLS-3)
--   • plans, departments — SELECT only (global operator catalogs, RLS-exempt)
--   • processed_events    — SELECT / INSERT / DELETE (consumer inserts,
--                           processed-events-prune cron deletes)
--   • outbox_events       — SELECT / INSERT / UPDATE / DELETE
--                           (outbox runner reads/updates/prunes; publisher
--                           inserts inside every business tx, EVT-10)
--
-- The `rls_violation_log` table is written to via `log_rls_violation()`,
-- which is called by `rls_check_tenant()` — SECURITY DEFINER — so the app
-- role does not need a direct grant on that table.
--
-- Idempotent: GRANT is additive and safe to re-run.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
        RAISE NOTICE 'org_membership_app role missing — skipping grants. In production Terraform provisions this role before migrations run.';
        RETURN;
    END IF;

    BEGIN
        -- ── Tenant-scoped tables (RLS-3 gates the writes) ─────────────────
        GRANT SELECT, INSERT, UPDATE, DELETE ON
            public.tenants,
            public.tenant_departments,
            public.tenant_memberships,
            public.tenant_roles,
            public.dept_memberships,
            public.dept_role_labels,
            public.group_dept_role_mappings,
            public.group_tenant_role_mappings,
            public.group_dept_mappings,
            public.delegations,
            public.tender_acl_entries,
            public.pending_invitations
        TO org_membership_app;

        -- ── Global catalogs (RLS-exempt) ──────────────────────────────────
        GRANT SELECT ON public.plans, public.departments TO org_membership_app;

        -- ── Housekeeping tables ───────────────────────────────────────────
        -- processed_events: consumer inserts on every applied event; the
        -- processed-events-prune cron deletes rows older than 8 days (PE-1).
        GRANT SELECT, INSERT, DELETE ON public.processed_events TO org_membership_app;

        -- outbox_events: created by platform-events; publisher inserts in
        -- every business tx (EVT-10); the outbox runner reads/marks/prunes.
        -- Guarded IF EXISTS because platform-events owns the migration and
        -- may not have run yet in an out-of-order dev bootstrap.
        IF EXISTS (SELECT 1 FROM information_schema.tables
                   WHERE table_schema = 'public' AND table_name = 'outbox_events') THEN
            GRANT SELECT, INSERT, UPDATE, DELETE ON public.outbox_events TO org_membership_app;
        END IF;
        IF EXISTS (SELECT 1 FROM information_schema.tables
                   WHERE table_schema = 'public' AND table_name = 'outbox_dead_letters') THEN
            GRANT SELECT, INSERT, UPDATE, DELETE ON public.outbox_dead_letters TO org_membership_app;
        END IF;

        -- ── Sequences on any of the above (uuid PKs use gen_random_uuid()
        -- rather than sequences, so no ALL SEQUENCES grant is needed today;
        -- keep this comment so a future serial-PK column adds an explicit
        -- GRANT rather than reaching for a blanket).
    EXCEPTION WHEN insufficient_privilege THEN
        RAISE NOTICE 'Grants to org_membership_app skipped — current user (%) lacks GRANT privilege. In production the migrator role has this; in dev the app role is the owner.', current_user;
    END;
END$$;
