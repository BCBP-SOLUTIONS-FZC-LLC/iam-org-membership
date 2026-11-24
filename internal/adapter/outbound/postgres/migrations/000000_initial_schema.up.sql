-- 000000_initial_schema — consolidated migration, single source of truth
-- for org_membership's current schema.
--
-- This service has never been deployed to any environment (LLD §19.1). The
-- schema has been squashed twice: first the 17 incremental pre-decomposition
-- migrations (000000_bootstrap through 000016_drop_delegation_tables) were
-- consolidated into this file; then the 10 additive migrations
-- (000001_check_constraints through 000010_seats_last_event_at) were folded
-- back in here before first deployment, since there is no live data to
-- preserve. See git history for the incremental path if ever needed.
-- Going forward, schema changes append NEW numbered migrations after this
-- one, following MIG-1's additive-then-destructive discipline.
--
-- Deliberately NOT carried forward — dead artifacts confirmed to have zero
-- live references once their owning table/column moved to a sibling service:
--   * branding_level ENUM — its only column, plans.custom_branding, moved
--     to the Catalog Service with the plans table.
--   * prevent_department_delete() / prevent_department_code_change() /
--     prevent_system_department_name_change() — trigger functions that
--     only ever attached to the departments table (Catalog Service).
--   * delegation_scope / delegation_status / tender_acl_level ENUMs and
--     the plans / departments / group_dept_role_mappings /
--     group_tenant_role_mappings / group_dept_mappings / delegations /
--     tender_acl_entries tables — extracted to sibling services under
--     ADR-0007/ADR-0008.
--   * trial_signup_ledger_email_hash_length (1-128 chars) — superseded by
--     the stricter trial_signup_ledger_email_hash_format (^[0-9a-f]{64}$).
--
-- End-state: 10 domain tables (tenants, tenant_departments,
-- tenant_memberships, tenant_roles, dept_memberships, dept_role_labels,
-- pending_invitations, processed_events, pending_tenant_events,
-- trial_signup_ledger) plus the rls_violation_log audit helper.
-- 8 tenant-scoped tables have FORCE ROW LEVEL SECURITY.
-- record_version optimistic locking on 7 tables (all but processed_events,
-- pending_tenant_events, trial_signup_ledger).
-- 8 enums.

-- -------------------------------------------------------------------------
-- Extensions
-- -------------------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS citext   WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;   -- gen_random_uuid()

-- -------------------------------------------------------------------------
-- Enums (8 types)
-- 'member' is DERIVED-ONLY per TR-7 / A29: never persisted in tenant_roles;
-- injected by I-8 into the effective role set at read time.
-- -------------------------------------------------------------------------
CREATE TYPE public.tenant_plan         AS ENUM ('starter', 'pro', 'enterprise');
CREATE TYPE public.subscription_status AS ENUM ('trial', 'active', 'past_due', 'cancelled', 'suspended', 'trial_expired', 'offboarded');
CREATE TYPE public.tenant_role         AS ENUM ('tenant_owner', 'tenant_admin', 'tender_admin', 'member');
CREATE TYPE public.membership_status   AS ENUM ('active', 'suspended', 'left');
CREATE TYPE public.dept_role           AS ENUM ('preparator', 'reviewer', 'approver');
CREATE TYPE public.realm_type          AS ENUM ('shared', 'dedicated');
CREATE TYPE public.invitation_status   AS ENUM ('pending', 'accepted', 'expired', 'revoked');
CREATE TYPE public.suspension_source   AS ENUM ('billing_lapse', 'operator');   -- T-16; resolves RP-11

-- -------------------------------------------------------------------------
-- app_tenant_id() — reads the tenant GUC set by pgcommon's GUC bridge.
-- STABLE (once per statement), SECURITY DEFINER, fail-closed (NULL on any
-- error so RLS blocks rather than silently opens).
-- -------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION public.app_tenant_id() RETURNS uuid
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = public AS $$
DECLARE v text;
BEGIN
    v := current_setting('app.tenant_id', true);
    IF v IS NULL OR v = '' THEN RETURN NULL; END IF;
    RETURN v::uuid;
EXCEPTION WHEN OTHERS THEN RETURN NULL;
END;
$$;

-- -------------------------------------------------------------------------
-- touch_row() — BEFORE UPDATE trigger on every record_version table (TRG-1..3).
-- WHEN (OLD.* IS DISTINCT FROM NEW.*) so a no-op UPDATE does not bump
-- record_version or updated_at (TRG-3).
-- -------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION public.touch_row() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at     := now();
    NEW.record_version := OLD.record_version + 1;
    RETURN NEW;
END;
$$;

-- -------------------------------------------------------------------------
-- rls_violation_log — sampled audit trail written by log_rls_violation()
-- from within RLS policy checks. RLS is DISABLED on this table (recursion guard).
-- -------------------------------------------------------------------------
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

-- -------------------------------------------------------------------------
-- log_rls_violation() — 1%-sampled INSERT into rls_violation_log. Silently
-- swallows its own errors so a logging failure cannot abort the caller's tx.
-- -------------------------------------------------------------------------
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

-- -------------------------------------------------------------------------
-- rls_check_tenant — STABLE STRICT SECURITY DEFINER. Returns true when the
-- row's tenant_id matches app.tenant_id GUC. Logs one of two violation
-- types on failure:
--   missing_or_invalid_guc — no GUC set / malformed UUID (RLS-2)
--   cross_tenant_access    — GUC present but different tenant (RLS-3)
-- -------------------------------------------------------------------------
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

-- -------------------------------------------------------------------------
-- tenants (T-1..T-16) — root aggregate. plan has no local FK (Catalog
-- Service owns the catalog); validated app-side against om:plans cache.
-- -------------------------------------------------------------------------
CREATE TABLE public.tenants (
    id                       uuid                        NOT NULL DEFAULT gen_random_uuid(),
    slug                     text                        NOT NULL,                        -- immutable (T-1)
    name                     text                        NOT NULL,
    plan                     public.tenant_plan          NOT NULL DEFAULT 'starter',
    feature_flags            jsonb                       NOT NULL DEFAULT '{}'::jsonb,    -- override delta only (T-9)
    status                   public.subscription_status  NOT NULL DEFAULT 'trial',
    trial_ends_at            timestamptz,                                                 -- required for trial/trial_expired (T-4)
    trial_reactivation_count int                         NOT NULL DEFAULT 0,              -- capped 0..1 (T-14)
    subscription_started_at  timestamptz,                                                 -- required for paid statuses (T-5)
    cancelled_at             timestamptz,                                                 -- biconditional with status (T-11); NOT set for operator suspension (T-16)
    suspension_source        public.suspension_source,                                    -- non-NULL iff status='suspended' (T-16)
    residency_mode           text                        NOT NULL DEFAULT 'standard',     -- ENUM standard|strict, all plans (CD-37)
    last_event_at            timestamptz,                                                 -- EVT-14 recency high-water (lifecycle events)
    seats_last_event_at      timestamptz,                                                 -- EVT-14 recency high-water for TenantSeatsChanged — independent of last_event_at (BL-OQ-2)
    realm_id                 text                        NOT NULL DEFAULT 'trial',
    realm_type               public.realm_type           NOT NULL DEFAULT 'shared',       -- T-6
    keycloak_shard           text                        NOT NULL DEFAULT 'shard-0',      -- T-12
    mfa_freshness_seconds    int                         NOT NULL DEFAULT 300,            -- 60..900 (T-10)
    local_accounts_enabled   boolean                     NOT NULL DEFAULT true,
    realm_sync_pending       boolean                     NOT NULL DEFAULT false,          -- T-15
    default_locale           text                        NOT NULL DEFAULT 'en-US',        -- BCP-47
    licensed_seats           int                         NOT NULL DEFAULT 10,             -- SEAT-1, T-8
    ownerless_since          timestamptz,                                                 -- T-13
    overage_since            timestamptz,                                                 -- SEAT-5
    record_version           bigint                      NOT NULL DEFAULT 1,
    created_at               timestamptz                 NOT NULL DEFAULT now(),
    updated_at               timestamptz                 NOT NULL DEFAULT now(),
    deleted_at               timestamptz,                                                 -- GDPR wipe only (T-7)
    CONSTRAINT tenants_pkey                       PRIMARY KEY (id),
    CONSTRAINT uq_tenants_slug                    UNIQUE (slug),
    CONSTRAINT tenants_slug_not_empty             CHECK (slug <> ''),
    CONSTRAINT tenants_name_not_empty             CHECK (name <> ''),
    CONSTRAINT tenants_realm_id_not_empty         CHECK (realm_id <> ''),
    CONSTRAINT tenants_keycloak_shard_not_empty   CHECK (keycloak_shard <> ''),
    CONSTRAINT tenants_reactivation_range         CHECK (trial_reactivation_count BETWEEN 0 AND 1),
    CONSTRAINT tenants_mfa_freshness_range        CHECK (mfa_freshness_seconds BETWEEN 60 AND 900),
    CONSTRAINT tenants_licensed_seats_positive    CHECK (licensed_seats > 0),
    CONSTRAINT tenants_record_version_positive    CHECK (record_version > 0),
    CONSTRAINT tenants_residency_mode_check       CHECK (residency_mode IN ('standard', 'strict')),
    CONSTRAINT chk_trial_ends_at_required
        CHECK (status NOT IN ('trial', 'trial_expired') OR trial_ends_at IS NOT NULL),
    CONSTRAINT chk_subscription_started_required
        CHECK (status IN ('trial', 'trial_expired', 'suspended') OR subscription_started_at IS NOT NULL),
    CONSTRAINT chk_offboarded_soft_deleted
        CHECK (status <> 'offboarded' OR deleted_at IS NOT NULL),
    CONSTRAINT chk_cancelled_at_required
        CHECK (
            CASE status
                WHEN 'cancelled'  THEN cancelled_at IS NOT NULL
                WHEN 'offboarded' THEN cancelled_at IS NOT NULL
                WHEN 'suspended'  THEN (suspension_source <> 'billing_lapse' OR cancelled_at IS NOT NULL)
                ELSE cancelled_at IS NULL
            END
        ),                                                                                -- T-11, CASE form (T-16)
    CONSTRAINT chk_suspension_source_required
        CHECK ((status = 'suspended') = (suspension_source IS NOT NULL))                 -- T-16
);

COMMENT ON COLUMN public.tenants.seats_last_event_at IS
    'EVT-14 recency high-water for TenantSeatsChanged — independent of last_event_at so seat changes cannot block lifecycle event processing (BL-OQ-2)';

-- -------------------------------------------------------------------------
-- tenant_departments — per-tenant activation junction for the global
-- department catalog (Catalog Service). No local FK on department_id.
-- -------------------------------------------------------------------------
CREATE TABLE public.tenant_departments (
    tenant_id      uuid        NOT NULL,
    department_id  uuid        NOT NULL,
    is_active      boolean     NOT NULL DEFAULT true,
    record_version bigint      NOT NULL DEFAULT 1,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tenant_departments_pkey                    PRIMARY KEY (tenant_id, department_id),
    CONSTRAINT fk_td_tenant                               FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT tenant_departments_record_version_positive CHECK (record_version > 0)
);

-- -------------------------------------------------------------------------
-- tenant_memberships — lifecycle-only, no role data (A14).
-- uq_tm_id_tenant_user is the composite FK target for tenant_roles and
-- dept_memberships (A15/A28); Tender ACL and Delegation call I-15 instead.
-- -------------------------------------------------------------------------
CREATE TABLE public.tenant_memberships (
    id             uuid                      NOT NULL DEFAULT gen_random_uuid(),
    tenant_id      uuid                      NOT NULL,
    user_id        uuid                      NOT NULL,
    status         public.membership_status  NOT NULL DEFAULT 'active',
    record_version bigint                    NOT NULL DEFAULT 1,
    created_at     timestamptz               NOT NULL DEFAULT now(),
    updated_at     timestamptz               NOT NULL DEFAULT now(),
    deleted_at     timestamptz,                            -- GDPR wipe only (TM-5)
    CONSTRAINT tenant_memberships_pkey                    PRIMARY KEY (id),
    CONSTRAINT fk_tm_tenant                               FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT tenant_memberships_record_version_positive CHECK (record_version > 0)
);

-- Non-partial composite unique required as FK target for tenant_roles and
-- dept_memberships. Kept non-partial so composite FKs remain enforceable
-- even when deleted_at is set (soft-deleted membership still pins child rows).
CREATE UNIQUE INDEX uq_tm_id_tenant_user ON public.tenant_memberships (id, tenant_id, user_id);

-- -------------------------------------------------------------------------
-- tenant_roles — elevated grants only. 'member' barred at DB level (TR-7).
-- -------------------------------------------------------------------------
CREATE TABLE public.tenant_roles (
    id                   uuid                NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid                NOT NULL,
    user_id              uuid                NOT NULL,
    tenant_membership_id uuid                NOT NULL,
    role_code            public.tenant_role  NOT NULL,
    granted_by           uuid                NOT NULL,
    record_version       bigint              NOT NULL DEFAULT 1,
    created_at           timestamptz         NOT NULL DEFAULT now(),
    updated_at           timestamptz         NOT NULL DEFAULT now(),
    deleted_at           timestamptz,
    CONSTRAINT tenant_roles_pkey                    PRIMARY KEY (id),
    CONSTRAINT fk_tnr_tenant                        FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT fk_tnr_tenant_membership             FOREIGN KEY (tenant_membership_id, tenant_id, user_id)
                                                    REFERENCES public.tenant_memberships (id, tenant_id, user_id),
    CONSTRAINT chk_tr_no_member                     CHECK (role_code <> 'member'),   -- TR-7
    CONSTRAINT tenant_roles_record_version_positive CHECK (record_version > 0)
);

-- -------------------------------------------------------------------------
-- dept_memberships — user x department x role_level.
-- -------------------------------------------------------------------------
CREATE TABLE public.dept_memberships (
    id                   uuid              NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            uuid              NOT NULL,
    user_id              uuid              NOT NULL,
    tenant_membership_id uuid              NOT NULL,
    department_id        uuid              NOT NULL,
    role_level           public.dept_role  NOT NULL,
    granted_by           uuid              NOT NULL,
    record_version       bigint            NOT NULL DEFAULT 1,
    created_at           timestamptz       NOT NULL DEFAULT now(),
    updated_at           timestamptz       NOT NULL DEFAULT now(),
    deleted_at           timestamptz,
    CONSTRAINT dept_memberships_pkey                    PRIMARY KEY (id),
    CONSTRAINT fk_dm_tenant                             FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT fk_dm_tenant_dept                        FOREIGN KEY (tenant_id, department_id)
                                                        REFERENCES public.tenant_departments (tenant_id, department_id),
    CONSTRAINT fk_dm_tenant_membership                  FOREIGN KEY (tenant_membership_id, tenant_id, user_id)
                                                        REFERENCES public.tenant_memberships (id, tenant_id, user_id),
    CONSTRAINT dept_memberships_record_version_positive CHECK (record_version > 0)
);

-- -------------------------------------------------------------------------
-- dept_role_labels — per-tenant display-name overrides for the 3 dept_role
-- values. Presentation only (DRL-2).
-- -------------------------------------------------------------------------
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
    CONSTRAINT dept_role_labels_display_name_not_empty  CHECK (display_name <> ''),
    CONSTRAINT chk_display_name_length                  CHECK (length(display_name) <= 255),   -- SECURITY-9
    CONSTRAINT dept_role_labels_record_version_positive CHECK (record_version > 0)
);

-- -------------------------------------------------------------------------
-- pending_invitations — two-step invite->accept staging (A11).
-- PII exception: email + full_name held until acceptance.
-- -------------------------------------------------------------------------
CREATE TABLE public.pending_invitations (
    id                    uuid                       NOT NULL DEFAULT gen_random_uuid(),
    tenant_id             uuid                       NOT NULL,
    email                 citext                     NOT NULL,
    full_name             text                       NOT NULL,
    initial_tenant_roles  public.tenant_role[]       NOT NULL DEFAULT '{}',
    initial_dept_mappings jsonb                      NOT NULL DEFAULT '[]'::jsonb,
    invited_by            uuid                       NOT NULL,
    keycloak_user_id      uuid,
    status                public.invitation_status   NOT NULL DEFAULT 'pending',
    expires_at            timestamptz                NOT NULL,
    accepted_at           timestamptz,
    kc_cleanup_pending    boolean                    NOT NULL DEFAULT false,   -- PI-9
    record_version        bigint                     NOT NULL DEFAULT 1,
    created_at            timestamptz                NOT NULL DEFAULT now(),
    updated_at            timestamptz                NOT NULL DEFAULT now(),
    CONSTRAINT pending_invitations_pkey                    PRIMARY KEY (id),
    CONSTRAINT fk_pi_tenant                                FOREIGN KEY (tenant_id) REFERENCES public.tenants(id) ON DELETE CASCADE,
    CONSTRAINT pi_full_name_not_empty                      CHECK (full_name <> ''),
    CONSTRAINT chk_full_name_length                        CHECK (length(full_name) <= 512),              -- SECURITY-2
    CONSTRAINT chk_pi_accepted_at_only_if_accepted         CHECK (accepted_at IS NULL OR status = 'accepted'),
    CONSTRAINT chk_pi_accepted_requires_at                 CHECK (status <> 'accepted' OR accepted_at IS NOT NULL),
    CONSTRAINT pending_invitations_record_version_positive CHECK (record_version > 0)
);

-- -------------------------------------------------------------------------
-- processed_events — SQS consumer dedup. No RLS, no record_version.
-- event_id is uuid (platform-events inbox shape). 8-day retention (PE-1).
-- -------------------------------------------------------------------------
CREATE TABLE public.processed_events (
    event_id     uuid        NOT NULL,
    consumer     text        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT processed_events_pkey PRIMARY KEY (event_id, consumer)
);

-- -------------------------------------------------------------------------
-- pending_tenant_events (CD-50, EVT-18) — park-and-replay staging for
-- events that arrive before the tenant row exists (e.g. a direct-paid
-- TenantConverted from RP-2 ahead of the Signup BFF's I-1). I-1 replays
-- them in occurred_at order on row creation; parked-events-replay CronJob
-- backstops any replay that never ran. No FK to tenants by design.
-- Tenant-scoped via RLS on app.tenant_id.
-- -------------------------------------------------------------------------
CREATE TABLE public.pending_tenant_events (
    event_id    uuid        NOT NULL,                  -- envelope id (same key as processed_events)
    tenant_id   uuid        NOT NULL,
    event_type  text        NOT NULL,
    occurred_at timestamptz NOT NULL,                  -- envelope time; replay order
    envelope    jsonb       NOT NULL,                  -- the envelope exactly as consumed
    parked_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pending_tenant_events_pkey PRIMARY KEY (event_id)
);

-- -------------------------------------------------------------------------
-- trial_signup_ledger (CR-4) — cross-tenant HMAC-SHA256 ledger for
-- one-trial-per-email enforcement. No RLS (pre-tenant by design).
-- Pruned after 730 days (trial-ledger-prune CronJob).
-- -------------------------------------------------------------------------
CREATE TABLE public.trial_signup_ledger (
    email_hash text        NOT NULL,
    tenant_id  uuid,                                      -- which tenant claimed; NULL for legacy rows
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT trial_signup_ledger_pkey PRIMARY KEY (email_hash),
    CONSTRAINT trial_signup_ledger_email_hash_format
        CHECK (email_hash ~ '^[0-9a-f]{64}$') NOT VALID  -- HMAC-SHA256 hex, lower-case, exactly 64 chars
);

-- -------------------------------------------------------------------------
-- Indexes
-- -------------------------------------------------------------------------
CREATE INDEX rls_violation_log_occurred_at_idx ON public.rls_violation_log (occurred_at);
CREATE INDEX idx_rls_violation_log_dedup       ON public.rls_violation_log (table_name, violation_type, occurred_at);

CREATE INDEX idx_tenants_status              ON public.tenants (status)       WHERE deleted_at IS NULL;
CREATE INDEX idx_tenants_trial_end           ON public.tenants (trial_ends_at) WHERE status = 'trial';
CREATE UNIQUE INDEX uq_tenants_realm_id_dedicated ON public.tenants (realm_id) WHERE realm_type = 'dedicated';
CREATE INDEX idx_tenants_ownerless           ON public.tenants (ownerless_since) WHERE ownerless_since IS NOT NULL;
CREATE INDEX idx_tenants_realm_sync_pending  ON public.tenants (updated_at)      WHERE realm_sync_pending;
CREATE INDEX idx_tenants_seat_overage        ON public.tenants (overage_since)   WHERE overage_since IS NOT NULL;

CREATE INDEX idx_tenant_departments_tenant     ON public.tenant_departments (tenant_id)     WHERE is_active = true;
CREATE INDEX idx_tenant_departments_department ON public.tenant_departments (department_id) WHERE is_active = true;

CREATE UNIQUE INDEX uq_tm_active_user      ON public.tenant_memberships (tenant_id, user_id) WHERE deleted_at IS NULL;
CREATE INDEX        idx_tm_tenant_id       ON public.tenant_memberships (tenant_id)          WHERE deleted_at IS NULL;
CREATE INDEX        idx_tm_user_id         ON public.tenant_memberships (user_id)            WHERE deleted_at IS NULL;
CREATE INDEX        idx_tm_status          ON public.tenant_memberships (tenant_id, status)  WHERE deleted_at IS NULL;
CREATE INDEX        idx_tm_tenant_created  ON public.tenant_memberships (tenant_id, created_at, id) WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX uq_tenant_roles_active       ON public.tenant_roles (tenant_id, user_id, role_code) WHERE deleted_at IS NULL;
CREATE INDEX        idx_tenant_roles_tenant_user  ON public.tenant_roles (tenant_id, user_id)           WHERE deleted_at IS NULL;
CREATE INDEX        idx_tenant_roles_role         ON public.tenant_roles (tenant_id, role_code)         WHERE deleted_at IS NULL;
CREATE INDEX        idx_tenant_roles_membership   ON public.tenant_roles (tenant_membership_id);

CREATE UNIQUE INDEX uq_dm_active_membership ON public.dept_memberships (tenant_id, user_id, department_id) WHERE deleted_at IS NULL;
CREATE INDEX        idx_dm_tenant_user      ON public.dept_memberships (tenant_id, user_id)                WHERE deleted_at IS NULL;
CREATE INDEX        idx_dm_tenant_dept      ON public.dept_memberships (tenant_id, department_id)          WHERE deleted_at IS NULL;
CREATE INDEX        idx_dm_user_role        ON public.dept_memberships (user_id, role_level)               WHERE deleted_at IS NULL;
CREATE INDEX        idx_dm_dept_role        ON public.dept_memberships (tenant_id, department_id, role_level) WHERE deleted_at IS NULL;
CREATE INDEX        idx_dm_tenant_membership ON public.dept_memberships (tenant_membership_id);

CREATE INDEX idx_dept_role_labels_tenant ON public.dept_role_labels (tenant_id);

CREATE UNIQUE INDEX uq_pi_pending       ON public.pending_invitations (tenant_id, email) WHERE status = 'pending';
CREATE INDEX idx_pi_tenant_pending      ON public.pending_invitations (tenant_id)        WHERE status = 'pending';
CREATE INDEX idx_pi_expiry              ON public.pending_invitations (expires_at)       WHERE status = 'pending';
CREATE INDEX idx_pi_keycloak_user       ON public.pending_invitations (keycloak_user_id) WHERE status = 'pending';
CREATE INDEX idx_pi_kc_cleanup          ON public.pending_invitations (id)               WHERE kc_cleanup_pending;
-- PUB-1: email-only lookup for pre-login tenant/realm resolution (5.4).
CREATE INDEX idx_pi_email_pending       ON public.pending_invitations (email)            WHERE status = 'pending';

CREATE INDEX idx_processed_events_prune ON public.processed_events (processed_at);

-- pending_tenant_events: replay sweep by (tenant, time); backstop prune by age.
CREATE INDEX idx_pte_tenant_order ON public.pending_tenant_events (tenant_id, occurred_at, event_id);
CREATE INDEX idx_pte_parked_at    ON public.pending_tenant_events (parked_at);

-- trial_signup_ledger: retention prune by age (trial-ledger-prune, 730 days).
CREATE INDEX idx_tsl_created_at ON public.trial_signup_ledger (created_at);

-- -------------------------------------------------------------------------
-- Triggers — touch_row() on the 7 record_version tables (TRG-1..3).
-- processed_events, pending_tenant_events, trial_signup_ledger excluded.
-- -------------------------------------------------------------------------
CREATE TRIGGER trg_touch_tenants
    BEFORE UPDATE ON public.tenants
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

CREATE TRIGGER trg_touch_pending_invitations
    BEFORE UPDATE ON public.pending_invitations
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

-- -------------------------------------------------------------------------
-- Immutability guard: tenants.slug (T-1).
-- -------------------------------------------------------------------------
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

-- -------------------------------------------------------------------------
-- pending_invitations.expires_at defense-in-depth (PI-1).
-- UPDATE is intentionally NOT gated — accept/revoke/expiry flows
-- legitimately transition rows after their expires_at has passed.
-- -------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION public.pending_invitations_expires_at_check() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.expires_at <= now() THEN
        RAISE EXCEPTION USING
            ERRCODE = 'check_violation',
            MESSAGE = 'pending_invitations.expires_at must be strictly in the future at insert time',
            DETAIL  = format('got expires_at=%s, now=%s', NEW.expires_at, now()),
            HINT    = 'invitation_service.Invite must compute expires_at = now() + INVITATION_EXPIRY_DAYS';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_pending_invitations_expiry_guard
    BEFORE INSERT ON public.pending_invitations
    FOR EACH ROW
    EXECUTE FUNCTION public.pending_invitations_expires_at_check();

-- -------------------------------------------------------------------------
-- Row-Level Security (RLS-1..RLS-6) — 8 tenant-scoped tables.
-- ENABLE + FORCE means even the table owner cannot bypass without BYPASSRLS.
-- rls_violation_log: RLS DISABLED (recursion guard).
-- trial_signup_ledger: no RLS (cross-tenant, pre-tenant by design).
-- -------------------------------------------------------------------------
ALTER TABLE public.tenants               ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenants               FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.tenant_departments    ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_departments    FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.tenant_memberships    ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_memberships    FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.tenant_roles          ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_roles          FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.dept_memberships      ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.dept_memberships      FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.dept_role_labels      ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.dept_role_labels      FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.pending_invitations   ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.pending_invitations   FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.pending_tenant_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.pending_tenant_events FORCE  ROW LEVEL SECURITY;

REVOKE ALL ON
    public.tenants,
    public.tenant_departments,
    public.tenant_memberships,
    public.tenant_roles,
    public.dept_memberships,
    public.dept_role_labels,
    public.pending_invitations,
    public.pending_tenant_events
FROM PUBLIC;

-- trial_signup_ledger: no RLS, but revoke PUBLIC to require explicit grants.
REVOKE ALL ON public.trial_signup_ledger FROM PUBLIC;

-- tenants policy uses id (the PK IS the tenant) rather than tenant_id.
CREATE POLICY tenant_isolation ON public.tenants
    USING      (rls_check_tenant(id, 'tenants'))
    WITH CHECK (rls_check_tenant(id, 'tenants'));

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

CREATE POLICY tenant_isolation ON public.pending_invitations
    USING      (rls_check_tenant(tenant_id, 'pending_invitations'))
    WITH CHECK (rls_check_tenant(tenant_id, 'pending_invitations'));

CREATE POLICY tenant_isolation ON public.pending_tenant_events
    USING      (rls_check_tenant(tenant_id, 'pending_tenant_events'))
    WITH CHECK (rls_check_tenant(tenant_id, 'pending_tenant_events'));

ALTER TABLE public.rls_violation_log DISABLE ROW LEVEL SECURITY;
ALTER TABLE public.rls_violation_log NO FORCE ROW LEVEL SECURITY;

-- -------------------------------------------------------------------------
-- Roles & grants (RLS-4 / MIG-3 / MIG-10 / MIG-11)
--
-- Three roles:
--   org_membership_app       — runtime, no BYPASSRLS (RLS-4)
--   org_membership_migrator  — migrations + reconciler + sysPool, BYPASSRLS
--   admin_readonly           — cross-tenant compliance reads, BYPASSRLS
--
-- In production Terraform provisions the roles; this migration attaches
-- grants and idempotently re-asserts BYPASSRLS.
-- -------------------------------------------------------------------------
DO $$
DECLARE
    v_can_create boolean;
BEGIN
    -- admin_readonly
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        BEGIN
            ALTER ROLE admin_readonly BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'admin_readonly exists but current user cannot ALTER ROLE — skipping BYPASSRLS refresh.';
        END;
    ELSE
        SELECT rolcreaterole OR rolsuper INTO v_can_create
          FROM pg_roles WHERE rolname = current_user;
        IF v_can_create THEN
            CREATE ROLE admin_readonly NOLOGIN BYPASSRLS;
        ELSE
            RAISE NOTICE 'admin_readonly role missing and current user (%) lacks CREATEROLE — skipping. In production Terraform provisions this role.', current_user;
        END IF;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        BEGIN
            REVOKE ALL ON
                public.tenants,
                public.tenant_departments,
                public.tenant_memberships,
                public.tenant_roles,
                public.dept_memberships,
                public.dept_role_labels,
                public.pending_invitations,
                public.pending_tenant_events,
                public.trial_signup_ledger
            FROM admin_readonly;

            GRANT SELECT ON
                public.tenants,
                public.tenant_departments,
                public.tenant_memberships,
                public.tenant_roles,
                public.dept_memberships,
                public.dept_role_labels,
                public.pending_invitations,
                public.pending_tenant_events,
                public.trial_signup_ledger,
                public.rls_violation_log,
                public.processed_events
            TO admin_readonly;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'Grants to admin_readonly skipped — current user (%) lacks GRANT privilege.', current_user;
        END;
    END IF;

    -- org_membership_migrator BYPASSRLS assertion
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator') THEN
        BEGIN
            ALTER ROLE org_membership_migrator BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'org_membership_migrator exists but current user cannot ALTER ROLE — skipping BYPASSRLS refresh.';
        END;
    END IF;

    -- org_membership_app defensive BYPASSRLS strip (RLS-4)
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app' AND rolbypassrls) THEN
        BEGIN
            ALTER ROLE org_membership_app NOBYPASSRLS;
            RAISE NOTICE 'Stripped BYPASSRLS from org_membership_app (RLS-4 invariant restored).';
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE WARNING 'org_membership_app has BYPASSRLS but current user cannot ALTER ROLE — RLS-4 VIOLATION requires operator action.';
        END;
    END IF;
END$$;

-- org_membership_app table grants
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
        RAISE NOTICE 'org_membership_app role missing — skipping grants. In production Terraform provisions this role.';
        RETURN;
    END IF;

    BEGIN
        GRANT SELECT, INSERT, UPDATE, DELETE ON
            public.tenants,
            public.tenant_departments,
            public.tenant_memberships,
            public.tenant_roles,
            public.dept_memberships,
            public.dept_role_labels,
            public.pending_invitations
        TO org_membership_app;

        GRANT SELECT, INSERT, DELETE ON public.processed_events          TO org_membership_app;
        GRANT SELECT, INSERT, DELETE ON public.pending_tenant_events      TO org_membership_app;
        GRANT SELECT, INSERT, UPDATE, DELETE ON public.trial_signup_ledger TO org_membership_app;

        -- outbox_events / outbox_dead_letters: platform-events owned; guard IF EXISTS
        -- for out-of-order dev bootstrap.
        IF EXISTS (SELECT 1 FROM information_schema.tables
                   WHERE table_schema = 'public' AND table_name = 'outbox_events') THEN
            GRANT SELECT, INSERT, UPDATE, DELETE ON public.outbox_events TO org_membership_app;
        END IF;
        IF EXISTS (SELECT 1 FROM information_schema.tables
                   WHERE table_schema = 'public' AND table_name = 'outbox_dead_letters') THEN
            GRANT SELECT, INSERT, UPDATE, DELETE ON public.outbox_dead_letters TO org_membership_app;
        END IF;
    EXCEPTION WHEN insufficient_privilege THEN
        RAISE NOTICE 'Grants to org_membership_app skipped — current user (%) lacks GRANT privilege.', current_user;
    END;
END$$;

-- -------------------------------------------------------------------------
-- org_membership_migrator grants (MIG-10) — DML on all non-owned tables.
-- rls_violation_log: SELECT only (SECURITY DEFINER writer must remain sole
-- writer). Default privileges cover tables created by future migrations.
-- No exception handler: a grant the running role cannot make fails loudly.
-- -------------------------------------------------------------------------
DO $$
DECLARE
    t record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator') THEN
        RAISE NOTICE '000000: org_membership_migrator does not exist — nothing to grant.';
        RETURN;
    END IF;
    FOR t IN
        SELECT tablename, tableowner FROM pg_tables
        WHERE schemaname = 'public'
          AND tableowner <> 'org_membership_migrator'
          AND tablename  <> 'schema_migrations'
    LOOP
        IF t.tablename = 'rls_violation_log' THEN
            EXECUTE format('GRANT SELECT ON public.%I TO org_membership_migrator', t.tablename);
        ELSE
            EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.%I TO org_membership_migrator', t.tablename);
        END IF;
    END LOOP;
    IF current_user <> 'org_membership_migrator' THEN
        ALTER DEFAULT PRIVILEGES IN SCHEMA public
            GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO org_membership_migrator;
    END IF;
END
$$;

-- -------------------------------------------------------------------------
-- pending_audit_entries — RLS and grants (platform-audit outbox, MIG-8).
-- Created by auditoutbox.ApplySchema before domain migrations run. This
-- block adds what the library leaves to the adopter (platform-audit LLD 4.3).
-- Fails loudly if the table is missing.
-- -------------------------------------------------------------------------
DO $$
BEGIN
    IF to_regclass('public.pending_audit_entries') IS NULL THEN
        RAISE EXCEPTION '000000: pending_audit_entries does not exist — run auditoutbox.ApplySchema (postgres.MigrateSchema does) before the domain migrations';
    END IF;
END$$;

ALTER TABLE public.pending_audit_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.pending_audit_entries FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON public.pending_audit_entries;
CREATE POLICY tenant_isolation ON public.pending_audit_entries
    USING      (rls_check_tenant(tenant_id, 'pending_audit_entries'))
    WITH CHECK (rls_check_tenant(tenant_id, 'pending_audit_entries'));

REVOKE ALL ON public.pending_audit_entries FROM PUBLIC;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
        BEGIN
            GRANT SELECT, INSERT ON public.pending_audit_entries TO org_membership_app;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'Grant to org_membership_app skipped — current user (%) lacks GRANT privilege.', current_user;
        END;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        BEGIN
            GRANT SELECT ON public.pending_audit_entries TO admin_readonly;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'Grant to admin_readonly skipped — current user (%) lacks GRANT privilege.', current_user;
        END;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator')
       AND (SELECT tableowner FROM pg_tables
            WHERE schemaname = 'public' AND tablename = 'pending_audit_entries') <> 'org_membership_migrator' THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON public.pending_audit_entries TO org_membership_migrator;
    END IF;
END$$;

-- -------------------------------------------------------------------------
-- Sequence grants (MIG-11) — platform-events' outbox_events_ordering_seq.
-- Every keyed enqueue calls nextval() on this sequence inside the business
-- transaction. Default privileges cover any future platform-events sequences.
-- No exception handler: a grant the running role cannot make fails loudly.
-- -------------------------------------------------------------------------
DO $$
DECLARE
    seq_owner name;
BEGIN
    SELECT pg_get_userbyid(c.relowner) INTO seq_owner
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'outbox_events_ordering_seq' AND c.relkind = 'S';

    IF seq_owner IS NULL THEN
        RAISE NOTICE '000000: outbox_events_ordering_seq does not exist — platform-events schema not applied yet; skipping sequence grant.';
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
        IF seq_owner IS NOT NULL AND seq_owner <> 'org_membership_app' THEN
            GRANT USAGE, SELECT ON SEQUENCE public.outbox_events_ordering_seq TO org_membership_app;
        END IF;
        IF current_user <> 'org_membership_app' THEN
            ALTER DEFAULT PRIVILEGES IN SCHEMA public
                GRANT USAGE, SELECT ON SEQUENCES TO org_membership_app;
        END IF;
    ELSE
        RAISE NOTICE '000000: org_membership_app does not exist — nothing to grant.';
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator') THEN
        IF seq_owner IS NOT NULL AND seq_owner <> 'org_membership_migrator' THEN
            GRANT USAGE, SELECT ON SEQUENCE public.outbox_events_ordering_seq TO org_membership_migrator;
        END IF;
        IF current_user <> 'org_membership_migrator' THEN
            ALTER DEFAULT PRIVILEGES IN SCHEMA public
                GRANT USAGE, SELECT ON SEQUENCES TO org_membership_migrator;
        END IF;
    END IF;
END
$$;
