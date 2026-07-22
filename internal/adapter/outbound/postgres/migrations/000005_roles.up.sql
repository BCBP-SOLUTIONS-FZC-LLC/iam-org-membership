-- Phase 1 · Migration 000005_roles — LLD §4.3 / §4.4 (BYPASSRLS + grants)
--
-- Three DB roles in play:
--   • org_membership_app       — runtime app. Does NOT hold BYPASSRLS (RLS-4).
--   • org_membership_migrator  — migrations + reconciler. Holds BYPASSRLS.
--   • admin_readonly           — cross-tenant compliance/support SELECTs.
--                                NOLOGIN + BYPASSRLS. Grants SELECT only.
--
-- In production the roles are provisioned by Terraform (owns the CREATE ROLE
-- step outside migrations); this migration then attaches the correct grants
-- and idempotently re-asserts BYPASSRLS. In dev the migrator user typically
-- lacks CREATEROLE, so we skip creation gracefully and emit a NOTICE.
--
-- Idempotent + safe to re-run in every environment.

DO $$
DECLARE
    v_can_create boolean;
BEGIN
    -- ── admin_readonly ────────────────────────────────────────────────────
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        BEGIN
            ALTER ROLE admin_readonly BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'admin_readonly exists but current user cannot ALTER ROLE — skipping BYPASSRLS refresh (fine in dev).';
        END;
    ELSE
        SELECT rolcreaterole OR rolsuper
          INTO v_can_create
          FROM pg_roles
         WHERE rolname = current_user;

        IF v_can_create THEN
            CREATE ROLE admin_readonly NOLOGIN BYPASSRLS;
        ELSE
            RAISE NOTICE 'admin_readonly role missing and current user (%) lacks CREATEROLE — skipping role creation. In production this role is provisioned by Terraform per LLD §4.3.', current_user;
        END IF;
    END IF;

    -- Grants — only run if the role exists at this point.
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
                public.pending_invitations
            FROM admin_readonly;

            GRANT SELECT ON
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
            TO admin_readonly;

            -- Housekeeping tables — useful for compliance reads, but no writes.
            GRANT SELECT ON public.rls_violation_log, public.processed_events, public.plans, public.departments
                TO admin_readonly;

            EXECUTE $c$COMMENT ON ROLE admin_readonly IS
                'LLD §4.3 cross-tenant compliance/support reader. BYPASSRLS + SELECT only.'$c$;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'Grants to admin_readonly skipped — current user (%) lacks the GRANT privilege on one or more target tables. In production the migrator role has this.', current_user;
        END;
    END IF;

    -- ── org_membership_migrator BYPASSRLS assertion ────────────────────────
    -- The scripts/init-db.sql local dev bootstrap creates this role with
    -- BYPASSRLS. In production Terraform owns creation; this block idempotently
    -- re-asserts the flag in case it was ever revoked.
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator') THEN
        BEGIN
            ALTER ROLE org_membership_migrator BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'org_membership_migrator exists but current user cannot ALTER ROLE — skipping BYPASSRLS refresh.';
        END;
    END IF;

    -- ── org_membership_app defensive check ─────────────────────────────────
    -- If org_membership_app somehow acquired BYPASSRLS (misconfiguration),
    -- strip it here. RLS-4 requires this role to be RLS-scoped.
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app' AND rolbypassrls) THEN
        BEGIN
            ALTER ROLE org_membership_app NOBYPASSRLS;
            RAISE NOTICE 'Stripped BYPASSRLS from org_membership_app (RLS-4 invariant restored).';
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE WARNING 'org_membership_app has BYPASSRLS but current user cannot ALTER ROLE — RLS-4 VIOLATION requires operator action.';
        END;
    END IF;
END$$;
