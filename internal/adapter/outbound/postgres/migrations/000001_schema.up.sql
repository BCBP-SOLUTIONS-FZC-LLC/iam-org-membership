-- Phase 1 · Migration 000001_schema — LLD §4
--
-- Full schema in a single migration (mirrors sibling iam-user-profile2's
-- 5-file pattern). This file owns:
--   • Extensions (citext, pgcrypto)
--   • 11 enums (§4.1)
--   • app_tenant_id() helper — safely reads app.tenant_id GUC
--   • touch_row() function — BEFORE UPDATE trigger body (§4.5)
--   • rls_violation_log table (RLS deliberately DISABLED to prevent recursion)
--   • Catalog tables in FK-safe order: plans (with seed), departments (with seed)
--   • Tenant tables in FK-safe order (§19.5 hard-dependency ordering):
--       tenants → tenant_departments → tenant_memberships (defines
--       uq_tm_id_tenant_user composite FK target) → tenant_roles →
--       dept_memberships → dept_role_labels → 3 group mapping tables →
--       delegations → tender_acl_entries → pending_invitations
--   • processed_events (operational, no RLS)
--
-- Indexes live in 000002; triggers in 000003; RLS policies in 000004;
-- admin_readonly role + grants in 000005.

-- ─────────────────────────────────────────────────────────────────────────
-- Extensions
-- ─────────────────────────────────────────────────────────────────────────
CREATE EXTENSION IF NOT EXISTS citext   WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;   -- gen_random_uuid()

-- ─────────────────────────────────────────────────────────────────────────
-- Enums (§4.1) — 11 types
-- ─────────────────────────────────────────────────────────────────────────
-- 'member' is DERIVED-ONLY per TR-7 / §16 A29: never persisted in
-- tenant_roles; injected by I-8 into the effective role set. Kept in the
-- enum so both the wire and the derived value share a domain.
CREATE TYPE public.tenant_plan         AS ENUM ('starter', 'pro', 'enterprise');
CREATE TYPE public.subscription_status AS ENUM ('trial', 'active', 'past_due', 'cancelled', 'suspended', 'trial_expired', 'offboarded');
CREATE TYPE public.tenant_role         AS ENUM ('tenant_owner', 'tenant_admin', 'tender_admin', 'member');
CREATE TYPE public.membership_status   AS ENUM ('active', 'suspended', 'left');
CREATE TYPE public.dept_role           AS ENUM ('preparator', 'reviewer', 'approver');
CREATE TYPE public.delegation_scope    AS ENUM ('all', 'department', 'tender');
CREATE TYPE public.delegation_status   AS ENUM ('active', 'ended', 'cancelled');
CREATE TYPE public.tender_acl_level    AS ENUM ('view', 'edit', 'approve');       -- §16 A17/A32(c)
CREATE TYPE public.realm_type          AS ENUM ('shared', 'dedicated');           -- §16 A22
CREATE TYPE public.invitation_status   AS ENUM ('pending', 'accepted', 'expired', 'revoked'); -- §16 A11
CREATE TYPE public.branding_level      AS ENUM ('none', 'logo');                  -- §16 A19

-- ─────────────────────────────────────────────────────────────────────────
-- app_tenant_id() — reads the tenant GUC set by pgcommon's GUC bridge.
-- STABLE (evaluates once per statement), SECURITY DEFINER (invoker cannot
-- poison the search path), fail-closed (returns NULL on any error so RLS
-- blocks rather than silently opens). Ported from sibling iam-user-profile.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION public.app_tenant_id() RETURNS uuid
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = public AS $$
DECLARE v text;
BEGIN
    v := current_setting('app.tenant_id', true);
    IF v IS NULL OR v = '' THEN RETURN NULL; END IF;
    RETURN v::uuid;
EXCEPTION WHEN OTHERS THEN RETURN NULL;   -- fail-closed on any parse error
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────
-- touch_row() — attached as BEFORE UPDATE on every record_version-carrying
-- table (§4.5, TRG-1..3). Fires only WHEN (OLD.* IS DISTINCT FROM NEW.*)
-- so a no-op UPDATE does not bump the version or timestamp (TRG-3).
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION public.touch_row() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at     := now();
    NEW.record_version := OLD.record_version + 1;
    RETURN NEW;
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────
-- rls_violation_log — sampled audit trail written by log_rls_violation()
-- from within RLS policy checks. RLS is DISABLED on this table (see
-- 000004_rls.up.sql) so the logger cannot recurse into itself.
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.rls_violation_log (
    id               bigserial   PRIMARY KEY,
    table_name       text        NOT NULL,
    row_tenant_id    uuid,
    app_tenant_id    uuid,
    violation_type   text        NOT NULL,   -- 'missing_or_invalid_guc' | 'cross_tenant_access'
    user_id          uuid,
    session_role     text        DEFAULT SESSION_USER,
    client_addr      inet        DEFAULT inet_client_addr(),
    application_name text        DEFAULT current_setting('application_name', true),
    query_text       text,
    occurred_at      timestamptz NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────────────────────
-- log_rls_violation() — 1%-sampled INSERT into rls_violation_log. Silently
-- swallows its own errors so a logging failure can never abort the
-- caller's transaction. Called from rls_check_tenant() in 000004_rls.up.sql.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION public.log_rls_violation(
    p_table_name     text,
    p_row_tenant_id  uuid,
    p_violation_type text
) RETURNS void
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = public AS $$
BEGIN
    IF random() > 0.01 THEN RETURN; END IF;
    INSERT INTO rls_violation_log (
        table_name, row_tenant_id, app_tenant_id, violation_type, query_text
    ) VALUES (
        p_table_name, p_row_tenant_id, app_tenant_id(), p_violation_type, current_query()
    );
EXCEPTION WHEN OTHERS THEN
    NULL;
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────
-- plans (§4.2, §16 A19) — global operator-editable entitlement catalog.
-- workflow_template_limit / tender_limit are NULLABLE with NULL = unlimited
-- (per LLD §19.3 recommendation, resolves the -1-sentinel ambiguity).
-- Must exist before tenants (fk_tenants_plan).
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.plans (
    code                    public.tenant_plan     NOT NULL,
    display_name            text                   NOT NULL,
    workflow_template_limit int,                                                -- NULL = unlimited
    tender_limit            int,                                                -- NULL = unlimited
    trial_duration_days     int                    NOT NULL,                    -- §16 A32(g)
    sso_enabled             boolean                NOT NULL DEFAULT false,
    custom_branding         public.branding_level  NOT NULL DEFAULT 'none',
    feature_set             jsonb                  NOT NULL DEFAULT '{}'::jsonb,
    record_version          bigint                 NOT NULL DEFAULT 1,
    created_at              timestamptz            NOT NULL DEFAULT now(),
    updated_at              timestamptz            NOT NULL DEFAULT now(),
    CONSTRAINT plans_pkey                    PRIMARY KEY (code),
    CONSTRAINT plans_display_name_not_empty  CHECK (display_name <> ''),
    CONSTRAINT plans_wf_limit_nonneg         CHECK (workflow_template_limit IS NULL OR workflow_template_limit >= 0),
    CONSTRAINT plans_tender_limit_nonneg     CHECK (tender_limit IS NULL OR tender_limit >= 0),
    CONSTRAINT plans_trial_days_nonneg       CHECK (trial_duration_days >= 0),
    CONSTRAINT plans_record_version_positive CHECK (record_version > 0)
);

-- 3 plan tiers seeded (§4.2 seed, adjusted for nullable-NULL-as-unlimited).
-- All tiers default to 30-day trials; operator can PATCH per-tier via O-6.
INSERT INTO public.plans (code, display_name, workflow_template_limit, tender_limit, trial_duration_days, sso_enabled, custom_branding, feature_set) VALUES
    ('starter',    'Starter',     5,    10,   30, false, 'none', '{}'::jsonb),
    ('pro',        'Pro',         50,   100,  30, false, 'logo', '{}'::jsonb),
    ('enterprise', 'Enterprise',  NULL, NULL, 30, true,  'logo', '{"require_mfa_all_users_allowed": true}'::jsonb)
ON CONFLICT (code) DO NOTHING;

-- ─────────────────────────────────────────────────────────────────────────
-- departments (§4.2) — global reference catalog. Operator-only writes;
-- never deleted (D-4 backstop in 000003_triggers). Retire via is_active=false.
-- Must exist before tenant_departments and dept_memberships.
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.departments (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    code           text        NOT NULL,                        -- immutable (D-10)
    name           text        NOT NULL,
    is_system      boolean     NOT NULL DEFAULT false,
    is_active      boolean     NOT NULL DEFAULT true,
    record_version bigint      NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT departments_pkey                    PRIMARY KEY (id),
    CONSTRAINT uq_departments_code                 UNIQUE (code),
    CONSTRAINT departments_code_not_empty          CHECK (code <> ''),
    CONSTRAINT departments_name_not_empty          CHECK (name <> ''),
    CONSTRAINT chk_system_department_active        CHECK (NOT (is_system = true AND is_active = false)),
    CONSTRAINT departments_record_version_positive CHECK (record_version > 0)
);

-- 5 system departments seeded per LLD §4.2 seed (authoritative — the
-- trial-signup workflow doc listing 4 was the outlier). §8.1 provisioning
-- activates all 5 for every new tenant via tenant_departments rows.
INSERT INTO public.departments (code, name, is_system) VALUES
    ('ENGINEERING', 'Engineering', true),
    ('DESIGN',      'Design',      true),
    ('PROCUREMENT', 'Procurement', true),
    ('FINANCE',     'Finance',     true),
    ('LEGAL',       'Legal',       true)
ON CONFLICT (code) DO NOTHING;

-- ─────────────────────────────────────────────────────────────────────────
-- tenants (§4.2) — root aggregate. FK to plans(code); slug immutable via
-- 000003_triggers. See T-1..T-15 for the full invariant set.
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.tenants (
    id                       uuid                        NOT NULL DEFAULT gen_random_uuid(),
    slug                     text                        NOT NULL,                                 -- immutable (T-1)
    name                     text                        NOT NULL,
    plan                     public.tenant_plan          NOT NULL DEFAULT 'starter',
    feature_flags            jsonb                       NOT NULL DEFAULT '{}'::jsonb,             -- override delta only (T-9, §16 A18)
    status                   public.subscription_status  NOT NULL DEFAULT 'trial',
    trial_ends_at            timestamptz,                                                          -- required for trial/trial_expired (T-4)
    trial_reactivation_count int                         NOT NULL DEFAULT 0,                       -- capped 0..1 (T-14, §16 A57)
    subscription_started_at  timestamptz,                                                          -- required for paid statuses (T-5)
    cancelled_at             timestamptz,                                                          -- biconditional with status (T-11, §16 A24)
    last_event_at            timestamptz,                                                          -- EVT-14 recency high-water (§16 A33)
    realm_id                 text                        NOT NULL DEFAULT 'trial',
    realm_type               public.realm_type           NOT NULL DEFAULT 'shared',                -- §16 A22, T-6
    keycloak_shard           text                        NOT NULL DEFAULT 'shard-0',               -- §16 A23, T-12
    mfa_freshness_seconds    int                         NOT NULL DEFAULT 300,                     -- 60..900 (T-10)
    local_accounts_enabled   boolean                     NOT NULL DEFAULT true,
    realm_sync_pending       boolean                     NOT NULL DEFAULT false,                   -- §16 A58, T-15
    default_locale           text                        NOT NULL DEFAULT 'en-US',                 -- BCP-47
    licensed_seats           int                         NOT NULL DEFAULT 10,                      -- SEAT-1, T-8
    ownerless_since          timestamptz,                                                          -- §16 A39, T-13
    overage_since            timestamptz,                                                          -- §16 A59, SEAT-5
    record_version           bigint                      NOT NULL DEFAULT 1,
    created_at               timestamptz                 NOT NULL DEFAULT now(),
    updated_at               timestamptz                 NOT NULL DEFAULT now(),
    deleted_at               timestamptz,                                                          -- GDPR wipe only (T-7)
    CONSTRAINT tenants_pkey                     PRIMARY KEY (id),
    CONSTRAINT uq_tenants_slug                  UNIQUE (slug),                                       -- full UNIQUE (T-6: slug retained across offboard)
    CONSTRAINT fk_tenants_plan                  FOREIGN KEY (plan) REFERENCES public.plans(code),
    CONSTRAINT tenants_slug_not_empty           CHECK (slug <> ''),
    CONSTRAINT tenants_name_not_empty           CHECK (name <> ''),
    CONSTRAINT tenants_realm_id_not_empty       CHECK (realm_id <> ''),
    CONSTRAINT tenants_keycloak_shard_not_empty CHECK (keycloak_shard <> ''),
    CONSTRAINT tenants_reactivation_range       CHECK (trial_reactivation_count BETWEEN 0 AND 1),
    CONSTRAINT tenants_mfa_freshness_range      CHECK (mfa_freshness_seconds BETWEEN 60 AND 900),
    CONSTRAINT tenants_licensed_seats_positive  CHECK (licensed_seats > 0),
    CONSTRAINT tenants_record_version_positive  CHECK (record_version > 0),
    CONSTRAINT chk_trial_ends_at_required
        CHECK (status NOT IN ('trial', 'trial_expired') OR trial_ends_at IS NOT NULL),
    CONSTRAINT chk_subscription_started_required
        CHECK (status IN ('trial', 'trial_expired') OR subscription_started_at IS NOT NULL),
    CONSTRAINT chk_offboarded_soft_deleted
        CHECK (status <> 'offboarded' OR deleted_at IS NOT NULL),                                    -- PAID-1
    CONSTRAINT chk_cancelled_at_required
        CHECK ((status IN ('cancelled', 'suspended', 'offboarded')) = (cancelled_at IS NOT NULL))    -- T-11
);

-- ─────────────────────────────────────────────────────────────────────────
-- tenant_departments (§4.2) — per-tenant activation of a global catalog
-- department. Composite PK (tenant_id, department_id) — no separate id
-- column, TD-7 makes the pair the identity.
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.tenant_departments (
    tenant_id      uuid        NOT NULL,
    department_id  uuid        NOT NULL,
    is_active      boolean     NOT NULL DEFAULT true,
    record_version bigint      NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tenant_departments_pkey                    PRIMARY KEY (tenant_id, department_id),
    CONSTRAINT fk_td_tenant                               FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT fk_td_department                           FOREIGN KEY (department_id) REFERENCES public.departments(id) ON DELETE RESTRICT,
    CONSTRAINT tenant_departments_record_version_positive CHECK (record_version > 0)
);

-- ─────────────────────────────────────────────────────────────────────────
-- tenant_memberships (§4.2, §16 A14) — lifecycle-only, no role data.
-- The uq_tm_id_tenant_user unique index (added in 000002) is the composite
-- FK target for tenant_roles, dept_memberships, delegations, and
-- tender_acl_entries (§16 A15/A16/A28/A31). MUST exist before those tables.
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.tenant_memberships (
    id             uuid                      NOT NULL DEFAULT gen_random_uuid(),
    tenant_id      uuid                      NOT NULL,
    user_id        uuid                      NOT NULL,                      -- Keycloak sub
    status         public.membership_status  NOT NULL DEFAULT 'active',
    record_version bigint                    NOT NULL DEFAULT 1,
    created_at     timestamptz               NOT NULL DEFAULT now(),
    updated_at     timestamptz               NOT NULL DEFAULT now(),
    deleted_at     timestamptz,                                             -- GDPR wipe only (TM-5)
    CONSTRAINT tenant_memberships_pkey                    PRIMARY KEY (id),
    CONSTRAINT fk_tm_tenant                               FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT tenant_memberships_record_version_positive CHECK (record_version > 0)
);

-- Non-partial composite unique — REQUIRED as FK target for the four child
-- tables. Kept non-partial so composite FKs remain enforceable even when
-- deleted_at is set (a soft-deleted membership still pins its child rows).
CREATE UNIQUE INDEX uq_tm_id_tenant_user ON public.tenant_memberships (id, tenant_id, user_id);

-- ─────────────────────────────────────────────────────────────────────────
-- tenant_roles (§4.2, §16 A14) — elevated grants only.
-- 'member' is barred at DB level (chk_tr_no_member, TR-7).
-- Composite FK targets uq_tm_id_tenant_user (§16 A31, TR-8).
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.tenant_roles (
    id                   uuid                NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid                NOT NULL,
    user_id              uuid                NOT NULL,
    tenant_membership_id uuid                NOT NULL,
    role_code            public.tenant_role  NOT NULL,
    granted_by           uuid                NOT NULL,                -- Keycloak sub of the granter (audit)
    record_version       bigint              NOT NULL DEFAULT 1,
    created_at           timestamptz         NOT NULL DEFAULT now(),
    updated_at           timestamptz         NOT NULL DEFAULT now(),
    deleted_at           timestamptz,                                 -- soft-delete on revoke
    CONSTRAINT tenant_roles_pkey                    PRIMARY KEY (id),
    CONSTRAINT fk_tnr_tenant                        FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT fk_tnr_tenant_membership             FOREIGN KEY (tenant_membership_id, tenant_id, user_id)
                                                    REFERENCES public.tenant_memberships (id, tenant_id, user_id),
    CONSTRAINT chk_tr_no_member                     CHECK (role_code <> 'member'),   -- TR-7 (§16 A29)
    CONSTRAINT tenant_roles_record_version_positive CHECK (record_version > 0)
);

-- ─────────────────────────────────────────────────────────────────────────
-- dept_memberships (§4.2) — user × department × role_level.
-- Composite FK to tenant_memberships (§16 A15/A28, DM-4).
-- Additional composite FK to tenant_departments so a user can only be
-- assigned to a dept the tenant has activated.
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.dept_memberships (
    id                   uuid              NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid              NOT NULL,
    user_id              uuid              NOT NULL,
    tenant_membership_id uuid              NOT NULL,
    department_id        uuid              NOT NULL,
    role_level           public.dept_role  NOT NULL,
    granted_by           uuid              NOT NULL,                   -- 'iam-system' UUID for JIT (§16 A32(e), DM-5)
    record_version       bigint            NOT NULL DEFAULT 1,
    created_at           timestamptz       NOT NULL DEFAULT now(),
    updated_at           timestamptz       NOT NULL DEFAULT now(),
    deleted_at           timestamptz,
    CONSTRAINT dept_memberships_pkey                    PRIMARY KEY (id),
    CONSTRAINT fk_dm_tenant                             FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT fk_dm_department                         FOREIGN KEY (department_id) REFERENCES public.departments(id) ON DELETE RESTRICT,
    CONSTRAINT fk_dm_tenant_dept                        FOREIGN KEY (tenant_id, department_id)
                                                        REFERENCES public.tenant_departments (tenant_id, department_id),
    CONSTRAINT fk_dm_tenant_membership                  FOREIGN KEY (tenant_membership_id, tenant_id, user_id)
                                                        REFERENCES public.tenant_memberships (id, tenant_id, user_id),
    CONSTRAINT dept_memberships_record_version_positive CHECK (record_version > 0)
);

-- ─────────────────────────────────────────────────────────────────────────
-- dept_role_labels (§4.2) — per-tenant display-name overrides for the 3
-- dept_role values. Presentation only (DRL-2). Three rows per tenant,
-- seeded at trial provisioning (§8.1).
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.dept_role_labels (
    id             uuid              NOT NULL DEFAULT gen_random_uuid(),
    tenant_id      uuid              NOT NULL,
    role_code      public.dept_role  NOT NULL,
    display_name   text              NOT NULL,
    record_version bigint            NOT NULL DEFAULT 1,
    created_at     timestamptz       NOT NULL DEFAULT now(),
    updated_at     timestamptz       NOT NULL DEFAULT now(),
    CONSTRAINT dept_role_labels_pkey                    PRIMARY KEY (id),
    CONSTRAINT uq_dept_role_labels                      UNIQUE (tenant_id, role_code),
    CONSTRAINT fk_drl_tenant                            FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT dept_role_labels_display_name_not_empty  CHECK (display_name <> ''),  -- defensive, LLD doesn't require but doesn't forbid
    CONSTRAINT dept_role_labels_record_version_positive CHECK (record_version > 0)
);

-- ─────────────────────────────────────────────────────────────────────────
-- group_dept_role_mappings (§4.2, §16 A25) — Keycloak group → dept_role.
-- Renamed from group_role_mappings when tenant-level role mappings were
-- split into a parallel table (below).
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.group_dept_role_mappings (
    id                  uuid              NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid              NOT NULL,
    keycloak_group_name text              NOT NULL,
    role_code           public.dept_role  NOT NULL,
    record_version      bigint            NOT NULL DEFAULT 1,
    created_at          timestamptz       NOT NULL DEFAULT now(),
    updated_at          timestamptz       NOT NULL DEFAULT now(),
    CONSTRAINT gdrm_pkey                    PRIMARY KEY (id),
    CONSTRAINT uq_group_dept_role_mapping   UNIQUE (tenant_id, keycloak_group_name),
    CONSTRAINT fk_gdrm_tenant               FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT gdrm_group_name_not_empty    CHECK (keycloak_group_name <> ''),
    CONSTRAINT gdrm_record_version_positive CHECK (record_version > 0)
);

-- ─────────────────────────────────────────────────────────────────────────
-- group_tenant_role_mappings (§4.2, §16 A25) — Keycloak group → tenant_role.
-- Parallel to group_dept_role_mappings but for elevated grants.
-- 'member' barred at DB level (chk_gtrm_no_member, GTRM-6).
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.group_tenant_role_mappings (
    id                  uuid                NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid                NOT NULL,
    keycloak_group_name text                NOT NULL,
    role_code           public.tenant_role  NOT NULL,
    record_version      bigint              NOT NULL DEFAULT 1,
    created_at          timestamptz         NOT NULL DEFAULT now(),
    updated_at          timestamptz         NOT NULL DEFAULT now(),
    CONSTRAINT gtrm_pkey                     PRIMARY KEY (id),
    CONSTRAINT uq_group_tenant_role_mapping  UNIQUE (tenant_id, keycloak_group_name),
    CONSTRAINT fk_gtrm_tenant                FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT chk_gtrm_no_member            CHECK (role_code <> 'member'),   -- GTRM-6 (§16 A29)
    CONSTRAINT gtrm_group_name_not_empty     CHECK (keycloak_group_name <> ''),
    CONSTRAINT gtrm_record_version_positive  CHECK (record_version > 0)
);

-- ─────────────────────────────────────────────────────────────────────────
-- group_dept_mappings (§4.2) — Keycloak group → department activation.
-- Distinct from group_dept_role_mappings: this table decides which depts a
-- group opens; the other decides at what role level.
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.group_dept_mappings (
    id                  uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id           uuid        NOT NULL,
    keycloak_group_name text        NOT NULL,
    department_id       uuid        NOT NULL,
    record_version      bigint      NOT NULL DEFAULT 1,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT gdm_pkey                    PRIMARY KEY (id),
    CONSTRAINT uq_group_dept_mapping       UNIQUE (tenant_id, keycloak_group_name, department_id),
    CONSTRAINT fk_gdm_tenant               FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT fk_gdm_department           FOREIGN KEY (department_id) REFERENCES public.departments(id) ON DELETE RESTRICT,
    CONSTRAINT gdm_group_name_not_empty    CHECK (keycloak_group_name <> ''),
    CONSTRAINT gdm_record_version_positive CHECK (record_version > 0)
);

-- ─────────────────────────────────────────────────────────────────────────
-- delegations (§4.2) — authoritative OOO grant. Both parties composite-FK'd
-- to tenant_memberships so the tenant_id / user_id triple is enforced at the
-- database (§16 A16, DEL-9). No unique on delegator (multiple concurrent
-- scoped delegations allowed per user).
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.delegations (
    id                      uuid                       NOT NULL DEFAULT gen_random_uuid(),
    tenant_id               uuid                       NOT NULL,
    delegator_id            uuid                       NOT NULL,
    delegate_id             uuid                       NOT NULL,
    delegator_membership_id uuid                       NOT NULL,
    delegate_membership_id  uuid                       NOT NULL,
    scope                   public.delegation_scope    NOT NULL DEFAULT 'all',
    scope_id                uuid,                                                  -- required when scope <> 'all'
    reason                  text,                                                  -- §16 A32(f), audit-only, 500-char cap service-side
    starts_at               timestamptz                NOT NULL DEFAULT now(),
    ends_at                 timestamptz,                                           -- open-ended allowed (DEL-8)
    status                  public.delegation_status   NOT NULL DEFAULT 'active',
    record_version          bigint                     NOT NULL DEFAULT 1,
    created_at              timestamptz                NOT NULL DEFAULT now(),
    updated_at              timestamptz                NOT NULL DEFAULT now(),
    deleted_at              timestamptz,
    CONSTRAINT delegations_pkey                    PRIMARY KEY (id),
    CONSTRAINT fk_del_tenant                       FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT fk_del_delegator_membership         FOREIGN KEY (delegator_membership_id, tenant_id, delegator_id)
                                                   REFERENCES public.tenant_memberships (id, tenant_id, user_id),
    CONSTRAINT fk_del_delegate_membership          FOREIGN KEY (delegate_membership_id, tenant_id, delegate_id)
                                                   REFERENCES public.tenant_memberships (id, tenant_id, user_id),
    CONSTRAINT chk_no_self_delegate                CHECK (delegator_id <> delegate_id),                    -- DEL-1
    CONSTRAINT chk_scope_id
        CHECK ((scope = 'all' AND scope_id IS NULL) OR (scope IN ('department', 'tender') AND scope_id IS NOT NULL)), -- DEL-2
    CONSTRAINT chk_ends_after_starts               CHECK (ends_at IS NULL OR ends_at > starts_at),        -- DEL-8 (strict >)
    CONSTRAINT delegations_record_version_positive CHECK (record_version > 0)
);

-- ─────────────────────────────────────────────────────────────────────────
-- tender_acl_entries (§4.2) — additive per-user overlay ACL for restricted
-- tenders. NO FK on tender_id (Tender Service owns tenders, cross-service).
-- Composite FK on membership (§16 A16, TAE-8).
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.tender_acl_entries (
    id                   uuid                     NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid                     NOT NULL,
    tender_id            uuid                     NOT NULL,                                -- NO FK: cross-service (Tender Service owns tenders)
    user_id              uuid                     NOT NULL,
    tenant_membership_id uuid                     NOT NULL,
    access_level         public.tender_acl_level  NOT NULL DEFAULT 'view',                 -- §16 A32(c) — HLD-aligned view/edit/approve
    granted_by           uuid                     NOT NULL,                                -- §16 A27, TAE-6
    reason               text,                                                             -- §16 A27, audit-only, capped 500 chars service-side
    expires_at           timestamptz,                                                      -- §16 A27, TAE-7 — passive expiry, future-only at handler
    record_version       bigint                   NOT NULL DEFAULT 1,
    created_at           timestamptz              NOT NULL DEFAULT now(),
    updated_at           timestamptz              NOT NULL DEFAULT now(),
    deleted_at           timestamptz,
    CONSTRAINT tender_acl_entries_pkey                    PRIMARY KEY (id),
    CONSTRAINT fk_tae_tenant                              FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT fk_tae_tenant_membership                   FOREIGN KEY (tenant_membership_id, tenant_id, user_id)
                                                          REFERENCES public.tenant_memberships (id, tenant_id, user_id),
    CONSTRAINT tender_acl_entries_record_version_positive CHECK (record_version > 0)
);

-- ─────────────────────────────────────────────────────────────────────────
-- pending_invitations (§4.2, §16 A11) — two-step invite→accept staging.
-- Sole PII exception in the schema (email + full_name until acceptance).
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.pending_invitations (
    id                    uuid                       NOT NULL DEFAULT gen_random_uuid(),
    tenant_id             uuid                       NOT NULL,
    email                 citext                     NOT NULL,                          -- case-insensitive
    full_name             text                       NOT NULL,
    initial_tenant_roles  public.tenant_role[]       NOT NULL DEFAULT '{}',             -- ENUM array (native, no jsonb)
    initial_dept_mappings jsonb                      NOT NULL DEFAULT '[]'::jsonb,      -- [{department_id, level}]
    invited_by            uuid                       NOT NULL,                          -- inviting admin's Keycloak sub
    keycloak_user_id      uuid,                                                         -- set post-RP-CreateInvitedUser
    status                public.invitation_status   NOT NULL DEFAULT 'pending',
    expires_at            timestamptz                NOT NULL,                          -- 7d default (INVITATION_EXPIRY_DAYS)
    accepted_at           timestamptz,
    kc_cleanup_pending    boolean                    NOT NULL DEFAULT false,            -- §16 A34, PI-9
    record_version        bigint                     NOT NULL DEFAULT 1,
    created_at            timestamptz                NOT NULL DEFAULT now(),
    updated_at            timestamptz                NOT NULL DEFAULT now(),
    CONSTRAINT pending_invitations_pkey                    PRIMARY KEY (id),
    CONSTRAINT fk_pi_tenant                                FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT pi_full_name_not_empty                      CHECK (full_name <> ''),
    CONSTRAINT chk_pi_accepted_at_only_if_accepted         CHECK (accepted_at IS NULL OR status = 'accepted'),        -- PI-2 half
    CONSTRAINT chk_pi_accepted_requires_at                 CHECK (status <> 'accepted' OR accepted_at IS NOT NULL),   -- PI-2 other half
    CONSTRAINT pending_invitations_record_version_positive CHECK (record_version > 0)
);

-- ─────────────────────────────────────────────────────────────────────────
-- processed_events (§4.2) — SQS consumer dedup. PK (event_id, consumer).
-- NO RLS (global), NO record_version, NO deleted_at. 8-day retention
-- (PE-1 strictly > 7-day SQS lifetime; IDEMP-4).
-- ─────────────────────────────────────────────────────────────────────────
CREATE TABLE public.processed_events (
    event_id     text        NOT NULL,
    consumer     text        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT processed_events_pkey PRIMARY KEY (event_id, consumer)
);
