-- ADR-0007 Wave 2 (group-mapping-jit-config-service-lld.md §18) — "Contract".
--
-- group_dept_role_mappings / group_tenant_role_mappings / group_dept_mappings
-- are now owned by Group Mapping Service. This service's read path (I-10
-- JIT resolution) already goes through Group Mapping Service's GM-I1 behind
-- the om:grm/om:gdm/om:gtrm cache; the admin write paths (P-14/P-15/P-16/
-- P-17/P-29) and their handler/service/repository code have been removed.
--
-- This service is still pre-production (no live traffic, no external
-- callers to repoint), so the cutover was done in a single pass rather
-- than the staged 410-Gone/soak-period sequence a deployed service would
-- need — there is no Stage 3 in this history, only Stage 2 (this repo's
-- reads) followed directly by Stage 4 (this drop).
--
-- Dropping each table takes its RLS policy, indexes (idx_gdrm_tenant,
-- idx_gtrm_tenant, idx_gdm_tenant, idx_gdm_group), unique constraints
-- (uq_group_dept_role_mapping, uq_group_tenant_role_mapping,
-- uq_group_dept_mapping), the chk_gtrm_no_member CHECK, its touch trigger,
-- and the fk_gdrm_tenant/fk_gtrm_tenant/fk_gdm_tenant ON DELETE CASCADE FKs
-- with it — no separate cleanup needed. group_dept_mappings' fk_gdm_department
-- was already dropped in 000013 (departments moved to the Catalog service).
--
-- lock_timeout follows this repo's standard schema-change safety
-- convention so a long-held lock can't block other deploys.

SET lock_timeout = '5s';

DROP TABLE IF EXISTS public.group_dept_role_mappings;
DROP TABLE IF EXISTS public.group_tenant_role_mappings;
DROP TABLE IF EXISTS public.group_dept_mappings;

RESET lock_timeout;
