-- Migration-runbook Phase 4 (LLD §12 step 4) — "Contract".
--
-- departments and plans are now owned by the Catalog / Admin Config
-- Service. This service's read paths already went through
-- catalog-admin-config (migration-runbook Phase 2, ADR-0007 Wave 1), and
-- its write paths (O-1/O-2/O-3 departments, O-5/O-6 plans) plus their
-- repositories and port interfaces have been removed from
-- OperatorService/OperatorHandler.
--
-- The 4 FK constraints dropped below degrade to app-level checks against
-- the CatalogReader — each already validates department_id/plan against
-- the catalog before writing (GroupMappingService.ReplaceDept,
-- DeptMembershipService.Assign, DepartmentService.Activate/SetActive,
-- ProvisioningService.TrialSignup), so dropping these DB-level FKs does
-- not remove a real safety guarantee, only its enforcement location — the
-- guarantee now carries the CatalogReader's cache staleness bound (LLD §8)
-- instead of being instantaneous.
--
-- IRREVERSIBLE IN PRACTICE: the down migration recreates the plans/
-- departments tables (schema + seed rows only) — it CANNOT restore
-- tenant_departments/dept_memberships/group_dept_mappings rows that
-- referenced the original (now-gone) department UUIDs, since a fresh
-- INSERT generates new ones. Take a full snapshot or logical-replication
-- checkpoint before applying this in any environment with real tenant
-- data, per the LLD §12 step 4 rollback note.

ALTER TABLE public.tenants             DROP CONSTRAINT IF EXISTS fk_tenants_plan;
ALTER TABLE public.tenant_departments  DROP CONSTRAINT IF EXISTS fk_td_department;
ALTER TABLE public.dept_memberships    DROP CONSTRAINT IF EXISTS fk_dm_department;
ALTER TABLE public.group_dept_mappings DROP CONSTRAINT IF EXISTS fk_gdm_department;

-- Triggers/CHECKs owned by these tables (trg_touch_plans, trg_touch_departments,
-- trg_prevent_department_delete, trg_department_code_immutable,
-- trg_system_department_name_immutable, chk_system_department_active) drop
-- automatically with their table. Grants naming these tables
-- (000005_roles, 000006_app_grants) become no-ops once the tables are gone.

DROP TABLE IF EXISTS public.plans;
DROP TABLE IF EXISTS public.departments;
