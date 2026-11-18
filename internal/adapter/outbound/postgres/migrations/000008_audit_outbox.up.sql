-- 000008_audit_outbox — RLS and grants for platform-audit's outbox table.
--
-- pending_audit_entries is created and versioned by platform-audit itself
-- (auditoutbox.ApplySchema, tracked in platform_audit_schema_migrations),
-- which postgres.MigrateSchema runs BEFORE these domain migrations. This
-- migration only adds what the library leaves to the adopter
-- (platform-audit LLD §4.3):
--   • RLS: ENABLE + FORCE, REVOKE ALL FROM PUBLIC, tenant_isolation on
--     app.tenant_id. P-2/P-6/P-31 enqueue under the request's tenant GUC.
--     This service writes no platform-wide (…00b1) entries from the app
--     pool, so WITH CHECK is the plain tenant check.
--   • org_membership_app: SELECT, INSERT (Enqueue only; the app pool never
--     claims, marks or deletes).
--   • org_membership_migrator (the BYPASSRLS sysPool): SELECT, INSERT,
--     UPDATE, DELETE. The auditoutbox.Runner in cmd/server drains on the
--     sysPool, and the invitation-expiry CronJob enqueues on it; INSERT is
--     also needed for the purge's own audit entries (MIG-10).
--   • admin_readonly: SELECT.
--
-- Fails loudly when the table is missing: RLS added later than the table
-- would leave a window with none, so the library schema must come first.
-- Additive only (MIG-1).

DO $$
BEGIN
    IF to_regclass('public.pending_audit_entries') IS NULL THEN
        RAISE EXCEPTION '000008: pending_audit_entries does not exist — run auditoutbox.ApplySchema (postgres.MigrateSchema does) before the domain migrations';
    END IF;
END$$;

ALTER TABLE public.pending_audit_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.pending_audit_entries FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON public.pending_audit_entries;
CREATE POLICY tenant_isolation ON public.pending_audit_entries
    USING      (rls_check_tenant(tenant_id, 'pending_audit_entries'))
    WITH CHECK (rls_check_tenant(tenant_id, 'pending_audit_entries'));

REVOKE ALL ON public.pending_audit_entries FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
        BEGIN
            GRANT SELECT, INSERT ON public.pending_audit_entries TO org_membership_app;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'Grant to org_membership_app skipped — current user (%) lacks GRANT privilege.', current_user;
        END;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        BEGIN
            GRANT SELECT ON public.pending_audit_entries TO admin_readonly;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'Grant to admin_readonly skipped — current user (%) lacks GRANT privilege.', current_user;
        END;
    END IF;
    -- Like 000007: only when the migrator is not the owner, and without an
    -- exception handler, so a grant that cannot be made fails the migration
    -- instead of leaving the runner unable to drain.
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator')
       AND (SELECT tableowner FROM pg_tables
            WHERE schemaname = 'public' AND tablename = 'pending_audit_entries') <> 'org_membership_migrator' THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON public.pending_audit_entries TO org_membership_migrator;
    END IF;
END$$;
