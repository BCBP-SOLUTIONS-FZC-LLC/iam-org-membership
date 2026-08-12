-- Reverse migration 000006 — revoke the app-role grants.
--
-- In practice this is only exercised in test/dev rollback flows; production
-- migrations are forward-only per MIG-1. Kept for pgmigrate up/down parity.

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
        RETURN;
    END IF;

    BEGIN
        REVOKE SELECT, INSERT, UPDATE, DELETE ON
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
        FROM org_membership_app;

        REVOKE SELECT ON public.plans, public.departments FROM org_membership_app;
        REVOKE SELECT, INSERT, DELETE ON public.processed_events FROM org_membership_app;

        IF EXISTS (SELECT 1 FROM information_schema.tables
                   WHERE table_schema = 'public' AND table_name = 'outbox_events') THEN
            REVOKE SELECT, INSERT, UPDATE, DELETE ON public.outbox_events FROM org_membership_app;
        END IF;
        IF EXISTS (SELECT 1 FROM information_schema.tables
                   WHERE table_schema = 'public' AND table_name = 'outbox_dead_letters') THEN
            REVOKE SELECT, INSERT, UPDATE, DELETE ON public.outbox_dead_letters FROM org_membership_app;
        END IF;
    EXCEPTION WHEN insufficient_privilege THEN
        RAISE NOTICE 'Revokes for org_membership_app skipped — insufficient privilege.';
    END;
END$$;
