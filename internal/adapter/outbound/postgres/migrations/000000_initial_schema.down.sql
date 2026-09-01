-- Down migration for 000000_initial_schema. Drops in reverse dependency
-- order. Extensions intentionally left in place (may be used by other DBs
-- on the same instance; DROP EXTENSION requires ownership and is destructive).

-- ── outbox_events customization (reverse first — added last in up.sql) ──
DROP TRIGGER IF EXISTS trg_outbox_normalize_payload ON outbox_events;
DROP FUNCTION IF EXISTS outbox_normalize_payload();
ALTER TABLE outbox_events ALTER COLUMN payload TYPE jsonb USING payload::jsonb;

-- ── Domain tables (reverse FK order) ─────────────────────────────────────
DROP TABLE IF EXISTS public.processed_events;
DROP TABLE IF EXISTS public.pending_invitations;
DROP TABLE IF EXISTS public.dept_role_labels;
DROP TABLE IF EXISTS public.dept_memberships;
DROP TABLE IF EXISTS public.tenant_roles;
DROP TABLE IF EXISTS public.tenant_memberships;
DROP TABLE IF EXISTS public.tenant_departments;
DROP TABLE IF EXISTS public.tenants;
DROP TABLE IF EXISTS public.rls_violation_log;

-- ── Functions ─────────────────────────────────────────────────────────────
DROP FUNCTION IF EXISTS public.pending_invitations_expires_at_check();
DROP FUNCTION IF EXISTS public.prevent_slug_change();
DROP FUNCTION IF EXISTS public.rls_check_tenant(uuid, text);
DROP FUNCTION IF EXISTS public.log_rls_violation(text, uuid, text);
DROP FUNCTION IF EXISTS public.touch_row();
DROP FUNCTION IF EXISTS public.app_tenant_id();

-- ── Enums ─────────────────────────────────────────────────────────────────
DROP TYPE IF EXISTS public.suspension_source;
DROP TYPE IF EXISTS public.invitation_status;
DROP TYPE IF EXISTS public.realm_type;
DROP TYPE IF EXISTS public.dept_role;
DROP TYPE IF EXISTS public.membership_status;
DROP TYPE IF EXISTS public.tenant_role;
DROP TYPE IF EXISTS public.subscription_status;
DROP TYPE IF EXISTS public.tenant_plan;
