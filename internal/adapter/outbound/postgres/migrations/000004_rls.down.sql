-- Phase 1 · Down migration for 000004_rls.

DROP POLICY IF EXISTS tenant_isolation ON public.pending_invitations;
DROP POLICY IF EXISTS tenant_isolation ON public.tender_acl_entries;
DROP POLICY IF EXISTS tenant_isolation ON public.delegations;
DROP POLICY IF EXISTS tenant_isolation ON public.group_dept_mappings;
DROP POLICY IF EXISTS tenant_isolation ON public.group_tenant_role_mappings;
DROP POLICY IF EXISTS tenant_isolation ON public.group_dept_role_mappings;
DROP POLICY IF EXISTS tenant_isolation ON public.dept_role_labels;
DROP POLICY IF EXISTS tenant_isolation ON public.dept_memberships;
DROP POLICY IF EXISTS tenant_isolation ON public.tenant_roles;
DROP POLICY IF EXISTS tenant_isolation ON public.tenant_memberships;
DROP POLICY IF EXISTS tenant_isolation ON public.tenant_departments;
DROP POLICY IF EXISTS tenant_isolation ON public.tenants;

ALTER TABLE public.pending_invitations        NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.pending_invitations        DISABLE  ROW LEVEL SECURITY;
ALTER TABLE public.tender_acl_entries         NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.tender_acl_entries         DISABLE  ROW LEVEL SECURITY;
ALTER TABLE public.delegations                NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.delegations                DISABLE  ROW LEVEL SECURITY;
ALTER TABLE public.group_dept_mappings        NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.group_dept_mappings        DISABLE  ROW LEVEL SECURITY;
ALTER TABLE public.group_tenant_role_mappings NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.group_tenant_role_mappings DISABLE  ROW LEVEL SECURITY;
ALTER TABLE public.group_dept_role_mappings   NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.group_dept_role_mappings   DISABLE  ROW LEVEL SECURITY;
ALTER TABLE public.dept_role_labels           NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.dept_role_labels           DISABLE  ROW LEVEL SECURITY;
ALTER TABLE public.dept_memberships           NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.dept_memberships           DISABLE  ROW LEVEL SECURITY;
ALTER TABLE public.tenant_roles               NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_roles               DISABLE  ROW LEVEL SECURITY;
ALTER TABLE public.tenant_memberships         NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_memberships         DISABLE  ROW LEVEL SECURITY;
ALTER TABLE public.tenant_departments         NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_departments         DISABLE  ROW LEVEL SECURITY;
ALTER TABLE public.tenants                    NO FORCE ROW LEVEL SECURITY;
ALTER TABLE public.tenants                    DISABLE  ROW LEVEL SECURITY;

DROP FUNCTION IF EXISTS public.rls_check_tenant(uuid, text);
