-- Phase 1 · Migration 000003_triggers — LLD §4.5 + §4.2 (immutability guards)
--
-- Attaches touch_row() as BEFORE UPDATE on every record_version-carrying
-- table (TRG-1..3, 14 tables total). Adds three immutability guards on
-- tenants + departments (T-1, D-10, D-11) and the departments delete
-- backstop (D-4, OP-3). touch_row() itself is defined in 000001_schema.

-- ─────────────────────────────────────────────────────────────────────────
-- touch_row triggers — WHEN (OLD.* IS DISTINCT FROM NEW.*) so a no-op
-- UPDATE does not bump record_version or updated_at (TRG-3). processed_events
-- is deliberately excluded — it has no record_version.
-- ─────────────────────────────────────────────────────────────────────────
CREATE TRIGGER trg_touch_tenants
    BEFORE UPDATE ON public.tenants
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_plans
    BEFORE UPDATE ON public.plans
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_departments
    BEFORE UPDATE ON public.departments
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_tenant_departments
    BEFORE UPDATE ON public.tenant_departments
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_tenant_memberships
    BEFORE UPDATE ON public.tenant_memberships
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_tenant_roles
    BEFORE UPDATE ON public.tenant_roles
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_dept_memberships
    BEFORE UPDATE ON public.dept_memberships
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_dept_role_labels
    BEFORE UPDATE ON public.dept_role_labels
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_group_dept_role_mappings
    BEFORE UPDATE ON public.group_dept_role_mappings
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_group_tenant_role_mappings
    BEFORE UPDATE ON public.group_tenant_role_mappings
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_group_dept_mappings
    BEFORE UPDATE ON public.group_dept_mappings
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_delegations
    BEFORE UPDATE ON public.delegations
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_tender_acl_entries
    BEFORE UPDATE ON public.tender_acl_entries
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_pending_invitations
    BEFORE UPDATE ON public.pending_invitations
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

-- ─────────────────────────────────────────────────────────────────────────
-- Immutability guard: tenants.slug (T-1).
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION public.prevent_slug_change() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.slug <> NEW.slug THEN
        RAISE EXCEPTION 'tenant slug is immutable (old: %, attempted: %)', OLD.slug, NEW.slug;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_tenant_slug_immutable
    BEFORE UPDATE OF slug ON public.tenants
    FOR EACH ROW
    EXECUTE FUNCTION public.prevent_slug_change();

-- ─────────────────────────────────────────────────────────────────────────
-- BEFORE DELETE backstop on departments (D-4, OP-3).
-- Departments are never physically deleted; retire via is_active = false.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION public.prevent_department_delete() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'departments cannot be deleted (code: %, id: %); retire via is_active = false', OLD.code, OLD.id;
END;
$$;

CREATE TRIGGER trg_prevent_department_delete
    BEFORE DELETE ON public.departments
    FOR EACH ROW
    EXECUTE FUNCTION public.prevent_department_delete();

-- ─────────────────────────────────────────────────────────────────────────
-- Immutability guard: departments.code (D-10).
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION public.prevent_department_code_change() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.code <> NEW.code THEN
        RAISE EXCEPTION 'department code is immutable (old: %, attempted: %)', OLD.code, NEW.code;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_department_code_immutable
    BEFORE UPDATE OF code ON public.departments
    FOR EACH ROW
    EXECUTE FUNCTION public.prevent_department_code_change();

-- ─────────────────────────────────────────────────────────────────────────
-- Immutability guard: system-department name (D-11).
-- Operator can rename a non-system department but not a system one.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION public.prevent_system_department_name_change() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.is_system AND OLD.name <> NEW.name THEN
        RAISE EXCEPTION 'system department name is immutable (code: %, old: %, attempted: %)', OLD.code, OLD.name, NEW.name;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_system_department_name_immutable
    BEFORE UPDATE OF name ON public.departments
    FOR EACH ROW
    EXECUTE FUNCTION public.prevent_system_department_name_change();
