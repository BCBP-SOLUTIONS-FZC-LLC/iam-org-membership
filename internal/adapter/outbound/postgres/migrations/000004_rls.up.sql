-- Phase 1 · Migration 000004_rls — LLD §4.3 (RLS-1..RLS-6)
--
-- Row-Level Security is the second of three isolation layers (§10.1).
-- ENABLE + FORCE means even the table owner cannot bypass RLS without an
-- explicit BYPASSRLS role. WITH CHECK gates writes, USING gates reads;
-- both invoke rls_check_tenant() so violations get sampled-logged.
--
-- Special case: the tenants table policy compares against `id` (not
-- `tenant_id`) because tenants.id IS the tenant PK. Single-row visibility
-- per tenant.
--
-- rls_violation_log stays RLS-disabled (recursion guard — see comment
-- in log_rls_violation() in 000001).

-- ─────────────────────────────────────────────────────────────────────────
-- rls_check_tenant — STABLE STRICT SECURITY DEFINER. Returns true when the
-- row's tenant_id matches app.tenant_id GUC. Logs one of two violation
-- types on failure:
--   • missing_or_invalid_guc — no GUC set / malformed UUID (RLS-2)
--   • cross_tenant_access    — GUC present but points at a different tenant (RLS-3)
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION public.rls_check_tenant(
    p_tenant_id  uuid,
    p_table_name text
) RETURNS boolean
    LANGUAGE plpgsql STABLE STRICT SECURITY DEFINER
    SET search_path = public AS $$
DECLARE
    v_app uuid;
BEGIN
    v_app := app_tenant_id();
    IF v_app IS NULL THEN
        PERFORM log_rls_violation(p_table_name, p_tenant_id, 'missing_or_invalid_guc');
        RETURN false;
    END IF;
    IF p_tenant_id <> v_app THEN
        PERFORM log_rls_violation(p_table_name, p_tenant_id, 'cross_tenant_access');
        RETURN false;
    END IF;
    RETURN true;
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────
-- ENABLE + FORCE ROW LEVEL SECURITY on all 12 tenant-scoped tables.
-- The 3 remaining tables (plans, departments, processed_events) are global
-- and deliberately excluded (§4.3, RLS-1).
-- ─────────────────────────────────────────────────────────────────────────
ALTER TABLE public.tenants                    ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenants                    FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.tenant_departments         ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_departments         FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.tenant_memberships         ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_memberships         FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.tenant_roles               ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_roles               FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.dept_memberships           ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.dept_memberships           FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.dept_role_labels           ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.dept_role_labels           FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.group_dept_role_mappings   ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.group_dept_role_mappings   FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.group_tenant_role_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.group_tenant_role_mappings FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.group_dept_mappings        ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.group_dept_mappings        FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.delegations                ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.delegations                FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.tender_acl_entries         ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tender_acl_entries         FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.pending_invitations        ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.pending_invitations        FORCE  ROW LEVEL SECURITY;

-- Explicitly REVOKE from PUBLIC so a mis-provisioned role cannot silently
-- read tenant tables without going through the policy (§4.3, defense-in-depth).
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
FROM PUBLIC;

-- ─────────────────────────────────────────────────────────────────────────
-- Policies — USING gates reads, WITH CHECK gates writes.
--
-- The tenants table is special-cased: `id` IS the tenant PK, so the policy
-- keys on id rather than tenant_id (which doesn't exist as a column on
-- tenants). This gives single-row visibility per tenant — a caller can
-- only read/write their own tenants row.
-- ─────────────────────────────────────────────────────────────────────────
CREATE POLICY tenant_isolation ON public.tenants
    USING      (id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (id = current_setting('app.tenant_id', true)::uuid);

CREATE POLICY tenant_isolation ON public.tenant_departments
    USING      (rls_check_tenant(tenant_id, 'tenant_departments'))
    WITH CHECK (rls_check_tenant(tenant_id, 'tenant_departments'));

CREATE POLICY tenant_isolation ON public.tenant_memberships
    USING      (rls_check_tenant(tenant_id, 'tenant_memberships'))
    WITH CHECK (rls_check_tenant(tenant_id, 'tenant_memberships'));

CREATE POLICY tenant_isolation ON public.tenant_roles
    USING      (rls_check_tenant(tenant_id, 'tenant_roles'))
    WITH CHECK (rls_check_tenant(tenant_id, 'tenant_roles'));

CREATE POLICY tenant_isolation ON public.dept_memberships
    USING      (rls_check_tenant(tenant_id, 'dept_memberships'))
    WITH CHECK (rls_check_tenant(tenant_id, 'dept_memberships'));

CREATE POLICY tenant_isolation ON public.dept_role_labels
    USING      (rls_check_tenant(tenant_id, 'dept_role_labels'))
    WITH CHECK (rls_check_tenant(tenant_id, 'dept_role_labels'));

CREATE POLICY tenant_isolation ON public.group_dept_role_mappings
    USING      (rls_check_tenant(tenant_id, 'group_dept_role_mappings'))
    WITH CHECK (rls_check_tenant(tenant_id, 'group_dept_role_mappings'));

CREATE POLICY tenant_isolation ON public.group_tenant_role_mappings
    USING      (rls_check_tenant(tenant_id, 'group_tenant_role_mappings'))
    WITH CHECK (rls_check_tenant(tenant_id, 'group_tenant_role_mappings'));

CREATE POLICY tenant_isolation ON public.group_dept_mappings
    USING      (rls_check_tenant(tenant_id, 'group_dept_mappings'))
    WITH CHECK (rls_check_tenant(tenant_id, 'group_dept_mappings'));

CREATE POLICY tenant_isolation ON public.delegations
    USING      (rls_check_tenant(tenant_id, 'delegations'))
    WITH CHECK (rls_check_tenant(tenant_id, 'delegations'));

CREATE POLICY tenant_isolation ON public.tender_acl_entries
    USING      (rls_check_tenant(tenant_id, 'tender_acl_entries'))
    WITH CHECK (rls_check_tenant(tenant_id, 'tender_acl_entries'));

CREATE POLICY tenant_isolation ON public.pending_invitations
    USING      (rls_check_tenant(tenant_id, 'pending_invitations'))
    WITH CHECK (rls_check_tenant(tenant_id, 'pending_invitations'));

-- ─────────────────────────────────────────────────────────────────────────
-- rls_violation_log recursion guard — RLS DISABLED so log_rls_violation()
-- can INSERT without triggering another policy check.
-- ─────────────────────────────────────────────────────────────────────────
ALTER TABLE public.rls_violation_log DISABLE ROW LEVEL SECURITY;
ALTER TABLE public.rls_violation_log NO FORCE ROW LEVEL SECURITY;
