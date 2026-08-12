-- Phase 2 · Down migration for 000008_tenants_rls_audit_helper.
-- Restores the raw-current_setting form from 000004.

DROP POLICY IF EXISTS tenant_isolation ON public.tenants;

CREATE POLICY tenant_isolation ON public.tenants
    USING      (id = current_setting('app.tenant_id', true)::uuid)
    WITH CHECK (id = current_setting('app.tenant_id', true)::uuid);
