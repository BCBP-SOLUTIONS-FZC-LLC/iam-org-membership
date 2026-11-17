-- 000008_audit_outbox (down) — remove the RLS policy and grants this
-- migration added. The table itself belongs to platform-audit's own
-- migrations (platform_audit_schema_migrations) and is left in place.

DROP POLICY IF EXISTS tenant_isolation ON public.pending_audit_entries;
ALTER TABLE IF EXISTS public.pending_audit_entries NO FORCE ROW LEVEL SECURITY;
ALTER TABLE IF EXISTS public.pending_audit_entries DISABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF to_regclass('public.pending_audit_entries') IS NULL THEN
        RETURN;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
        REVOKE SELECT, INSERT ON public.pending_audit_entries FROM org_membership_app;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        REVOKE SELECT ON public.pending_audit_entries FROM admin_readonly;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator')
       AND (SELECT tableowner FROM pg_tables
            WHERE schemaname = 'public' AND tablename = 'pending_audit_entries') <> 'org_membership_migrator' THEN
        REVOKE SELECT, INSERT, UPDATE, DELETE ON public.pending_audit_entries FROM org_membership_migrator;
    END IF;
END$$;
