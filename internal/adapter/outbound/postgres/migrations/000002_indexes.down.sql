-- Phase 1 · Down migration for 000002_indexes. Dropped indexes are recreated
-- by the up migration; leaving them dropped is safe for a full teardown
-- (000001_schema.down.sql then follows and removes the tables entirely).

DROP INDEX IF EXISTS public.idx_processed_events_prune;

DROP INDEX IF EXISTS public.idx_pi_kc_cleanup;
DROP INDEX IF EXISTS public.idx_pi_keycloak_user;
DROP INDEX IF EXISTS public.idx_pi_expiry;
DROP INDEX IF EXISTS public.idx_pi_tenant_pending;
DROP INDEX IF EXISTS public.uq_pi_pending;

DROP INDEX IF EXISTS public.idx_tae_membership;
DROP INDEX IF EXISTS public.idx_tae_user;
DROP INDEX IF EXISTS public.idx_tae_tenant_tender;
DROP INDEX IF EXISTS public.uq_tae_active_entry;

DROP INDEX IF EXISTS public.idx_delegations_delegate_mem;
DROP INDEX IF EXISTS public.idx_delegations_delegator_mem;
DROP INDEX IF EXISTS public.idx_delegations_ends_at;
DROP INDEX IF EXISTS public.idx_delegations_delegate;
DROP INDEX IF EXISTS public.idx_delegations_delegator;
DROP INDEX IF EXISTS public.idx_delegations_tenant;

DROP INDEX IF EXISTS public.idx_gdm_group;
DROP INDEX IF EXISTS public.idx_gdm_tenant;
DROP INDEX IF EXISTS public.idx_gtrm_tenant;
DROP INDEX IF EXISTS public.idx_gdrm_tenant;

DROP INDEX IF EXISTS public.idx_dept_role_labels_tenant;

DROP INDEX IF EXISTS public.idx_dm_tenant_membership;
DROP INDEX IF EXISTS public.idx_dm_dept_role;
DROP INDEX IF EXISTS public.idx_dm_user_role;
DROP INDEX IF EXISTS public.idx_dm_tenant_dept;
DROP INDEX IF EXISTS public.idx_dm_tenant_user;
DROP INDEX IF EXISTS public.uq_dm_active_membership;

DROP INDEX IF EXISTS public.idx_tenant_roles_membership;
DROP INDEX IF EXISTS public.idx_tenant_roles_role;
DROP INDEX IF EXISTS public.idx_tenant_roles_tenant_user;
DROP INDEX IF EXISTS public.uq_tenant_roles_active;

DROP INDEX IF EXISTS public.idx_tm_tenant_created;
DROP INDEX IF EXISTS public.idx_tm_status;
DROP INDEX IF EXISTS public.idx_tm_user_id;
DROP INDEX IF EXISTS public.idx_tm_tenant_id;
DROP INDEX IF EXISTS public.uq_tm_active_user;

DROP INDEX IF EXISTS public.idx_tenant_departments_department;
DROP INDEX IF EXISTS public.idx_tenant_departments_tenant;

DROP INDEX IF EXISTS public.idx_tenants_seat_overage;
DROP INDEX IF EXISTS public.idx_tenants_realm_sync_pending;
DROP INDEX IF EXISTS public.idx_tenants_ownerless;
DROP INDEX IF EXISTS public.uq_tenants_realm_id_dedicated;
DROP INDEX IF EXISTS public.idx_tenants_trial_end;
DROP INDEX IF EXISTS public.idx_tenants_status;

DROP INDEX IF EXISTS public.idx_rls_violation_log_dedup;
DROP INDEX IF EXISTS public.rls_violation_log_occurred_at_idx;
