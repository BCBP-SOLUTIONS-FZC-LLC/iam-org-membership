-- Phase 1 · Down migration for 000005_roles.
--
-- Only the grants are reversed; the roles themselves are left in place
-- (Terraform owns them in production, and DROP ROLE requires REVOKE of every
-- object grant across every DB — outside a migration's scope).

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        BEGIN
            REVOKE ALL ON
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
                public.pending_invitations,
                public.rls_violation_log,
                public.processed_events,
                public.plans,
                public.departments
            FROM admin_readonly;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'admin_readonly REVOKE skipped — current user lacks privilege.';
        END;
    END IF;
END$$;
