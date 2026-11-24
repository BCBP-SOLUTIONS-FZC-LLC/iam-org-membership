-- Down migration for 000000_initial_schema (consolidated).
-- Drops everything this migration created, in reverse dependency order.
-- Extensions are intentionally left in place (may be used by other DBs on
-- the same instance; DROP EXTENSION requires ownership and is destructive).

-- ── Revoke default privileges on sequences (MIG-11) ──────────────────────
DO $$
DECLARE
    seq_owner name;
BEGIN
    SELECT pg_get_userbyid(c.relowner) INTO seq_owner
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'outbox_events_ordering_seq' AND c.relkind = 'S';

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
        IF current_user <> 'org_membership_app' THEN
            ALTER DEFAULT PRIVILEGES IN SCHEMA public
                REVOKE USAGE, SELECT ON SEQUENCES FROM org_membership_app;
        END IF;
        IF seq_owner IS NOT NULL AND seq_owner <> 'org_membership_app' THEN
            REVOKE USAGE, SELECT ON SEQUENCE public.outbox_events_ordering_seq FROM org_membership_app;
        END IF;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator') THEN
        IF current_user <> 'org_membership_migrator' THEN
            ALTER DEFAULT PRIVILEGES IN SCHEMA public
                REVOKE USAGE, SELECT ON SEQUENCES FROM org_membership_migrator;
        END IF;
        IF seq_owner IS NOT NULL AND seq_owner <> 'org_membership_migrator' THEN
            REVOKE USAGE, SELECT ON SEQUENCE public.outbox_events_ordering_seq FROM org_membership_migrator;
        END IF;
    END IF;
END
$$;

-- ── Revoke default privileges on tables (MIG-10) ─────────────────────────
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator')
       AND current_user <> 'org_membership_migrator' THEN
        ALTER DEFAULT PRIVILEGES IN SCHEMA public
            REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM org_membership_migrator;
    END IF;
END
$$;

-- ── pending_audit_entries: remove RLS policy and grants (MIG-8) ──────────
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

-- ── Domain tables (reverse FK / dependency order) ────────────────────────
DROP TABLE IF EXISTS public.pending_tenant_events;
DROP TABLE IF EXISTS public.trial_signup_ledger;
DROP TABLE IF EXISTS public.processed_events;
DROP TABLE IF EXISTS public.pending_invitations;
DROP TABLE IF EXISTS public.dept_role_labels;
DROP TABLE IF EXISTS public.dept_memberships;
DROP TABLE IF EXISTS public.tenant_roles;
DROP TABLE IF EXISTS public.tenant_memberships;
DROP TABLE IF EXISTS public.tenant_departments;
DROP TABLE IF EXISTS public.tenants;
DROP TABLE IF EXISTS public.rls_violation_log;

-- ── Functions ─────────────────────────────────────────────────────────────
DROP FUNCTION IF EXISTS public.pending_invitations_expires_at_check();
DROP FUNCTION IF EXISTS public.prevent_slug_change();
DROP FUNCTION IF EXISTS public.rls_check_tenant(uuid, text);
DROP FUNCTION IF EXISTS public.log_rls_violation(text, uuid, text);
DROP FUNCTION IF EXISTS public.touch_row();
DROP FUNCTION IF EXISTS public.app_tenant_id();

-- ── Enums ─────────────────────────────────────────────────────────────────
DROP TYPE IF EXISTS public.suspension_source;
DROP TYPE IF EXISTS public.invitation_status;
DROP TYPE IF EXISTS public.realm_type;
DROP TYPE IF EXISTS public.dept_role;
DROP TYPE IF EXISTS public.membership_status;
DROP TYPE IF EXISTS public.tenant_role;
DROP TYPE IF EXISTS public.subscription_status;
DROP TYPE IF EXISTS public.tenant_plan;
