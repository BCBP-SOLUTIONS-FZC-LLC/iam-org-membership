-- Phase 1 · Down migration for 000001_schema. Drops in reverse FK order.

DROP TABLE IF EXISTS public.processed_events;
DROP TABLE IF EXISTS public.pending_invitations;
DROP TABLE IF EXISTS public.tender_acl_entries;
DROP TABLE IF EXISTS public.delegations;
DROP TABLE IF EXISTS public.group_dept_mappings;
DROP TABLE IF EXISTS public.group_tenant_role_mappings;
DROP TABLE IF EXISTS public.group_dept_role_mappings;
DROP TABLE IF EXISTS public.dept_role_labels;
DROP TABLE IF EXISTS public.dept_memberships;
DROP TABLE IF EXISTS public.tenant_roles;
DROP TABLE IF EXISTS public.tenant_memberships;
DROP TABLE IF EXISTS public.tenant_departments;
DROP TABLE IF EXISTS public.tenants;
DROP TABLE IF EXISTS public.departments;
DROP TABLE IF EXISTS public.plans;
DROP TABLE IF EXISTS public.rls_violation_log;

DROP FUNCTION IF EXISTS public.log_rls_violation(text, uuid, text);
DROP FUNCTION IF EXISTS public.touch_row();
DROP FUNCTION IF EXISTS public.app_tenant_id();

DROP TYPE IF EXISTS public.branding_level;
DROP TYPE IF EXISTS public.invitation_status;
DROP TYPE IF EXISTS public.realm_type;
DROP TYPE IF EXISTS public.tender_acl_level;
DROP TYPE IF EXISTS public.delegation_status;
DROP TYPE IF EXISTS public.delegation_scope;
DROP TYPE IF EXISTS public.dept_role;
DROP TYPE IF EXISTS public.membership_status;
DROP TYPE IF EXISTS public.tenant_role;
DROP TYPE IF EXISTS public.subscription_status;
DROP TYPE IF EXISTS public.tenant_plan;

-- Extensions intentionally left in place (may be used by other DBs on the
-- same instance; DROP EXTENSION requires ownership and is destructive).
