-- Phase 1 · Down migration for 000003_triggers.

DROP TRIGGER IF EXISTS trg_system_department_name_immutable ON public.departments;
DROP TRIGGER IF EXISTS trg_department_code_immutable        ON public.departments;
DROP TRIGGER IF EXISTS trg_prevent_department_delete        ON public.departments;
DROP TRIGGER IF EXISTS trg_tenant_slug_immutable            ON public.tenants;

DROP TRIGGER IF EXISTS trg_touch_pending_invitations         ON public.pending_invitations;
DROP TRIGGER IF EXISTS trg_touch_tender_acl_entries          ON public.tender_acl_entries;
DROP TRIGGER IF EXISTS trg_touch_delegations                 ON public.delegations;
DROP TRIGGER IF EXISTS trg_touch_group_dept_mappings         ON public.group_dept_mappings;
DROP TRIGGER IF EXISTS trg_touch_group_tenant_role_mappings  ON public.group_tenant_role_mappings;
DROP TRIGGER IF EXISTS trg_touch_group_dept_role_mappings    ON public.group_dept_role_mappings;
DROP TRIGGER IF EXISTS trg_touch_dept_role_labels            ON public.dept_role_labels;
DROP TRIGGER IF EXISTS trg_touch_dept_memberships            ON public.dept_memberships;
DROP TRIGGER IF EXISTS trg_touch_tenant_roles                ON public.tenant_roles;
DROP TRIGGER IF EXISTS trg_touch_tenant_memberships          ON public.tenant_memberships;
DROP TRIGGER IF EXISTS trg_touch_tenant_departments          ON public.tenant_departments;
DROP TRIGGER IF EXISTS trg_touch_departments                 ON public.departments;
DROP TRIGGER IF EXISTS trg_touch_plans                       ON public.plans;
DROP TRIGGER IF EXISTS trg_touch_tenants                     ON public.tenants;

DROP FUNCTION IF EXISTS public.prevent_system_department_name_change();
DROP FUNCTION IF EXISTS public.prevent_department_code_change();
DROP FUNCTION IF EXISTS public.prevent_department_delete();
DROP FUNCTION IF EXISTS public.prevent_slug_change();
