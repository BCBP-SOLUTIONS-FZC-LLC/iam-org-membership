-- Phase 2 · Migration 000008 — route tenants RLS policy through
-- rls_check_tenant() so cross-tenant reads/writes on the root table
-- fire iam_rls_violations_total instead of throwing a raw ::uuid cast
-- error on missing GUC. Fixes an audit blindspot on the tenants table
-- surfaced during LLD compliance review.
--
-- All other 11 tenant-scoped tables already route through
-- rls_check_tenant() (see 000004_rls.up.sql:104-...); tenants was
-- special-cased because `id` IS the tenant PK, but the helper signature
-- takes any UUID + a table name and doesn't care that the column is
-- named `id` rather than `tenant_id`.
--
-- LLD §16 A18 (RLS-2/RLS-3 audit trail) + §11.1 (rls_violation_log).

DROP POLICY IF EXISTS tenant_isolation ON public.tenants;

CREATE POLICY tenant_isolation ON public.tenants
    USING      (rls_check_tenant(id, 'tenants'))
    WITH CHECK (rls_check_tenant(id, 'tenants'));
