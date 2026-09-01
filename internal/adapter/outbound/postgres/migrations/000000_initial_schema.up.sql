-- 000000_initial_schema — consolidated migration, single source of truth
-- for org_membership's current schema.
--
-- This service has never been deployed to any environment (LLD §19.1), so
-- the 17 incremental migrations that built this schema up to now
-- (000000_bootstrap through 000016_drop_delegation_tables — base schema,
-- indexes, triggers, RLS, roles/grants, five hotfixes, and four
-- table-group extractions to sibling services under ADR-0007/ADR-0008)
-- have been squashed into this single file. There is no live data or
-- deployed environment to preserve, so a clean single migration is more
-- useful than 17 files of dead history. See git history (tag/commit prior
-- to this squash) if the incremental path is ever needed for reference.
-- Going forward, schema changes append NEW numbered migrations after this
-- one, following MIG-1's additive-then-destructive discipline.
--
-- Deliberately NOT carried forward from the incremental history — dead
-- artifacts confirmed to have zero live references (via repo-wide grep)
-- once their owning table/column moved to a sibling service:
--   • branding_level ENUM — its only column, plans.custom_branding, moved
--     to the Catalog Service with the plans table; no other column ever
--     referenced it.
--   • prevent_department_delete() / prevent_department_code_change() /
--     prevent_system_department_name_change() — trigger functions that
--     only ever attached to the departments table, which moved to the
--     Catalog Service.
--   • delegation_scope / delegation_status / tender_acl_level ENUMs and
--     the plans / departments / group_dept_role_mappings /
--     group_tenant_role_mappings / group_dept_mappings / delegations /
--     tender_acl_entries tables — all extracted to sibling services
--     (Catalog / Admin Config, Group Mapping / JIT Config, Tender ACL,
--     Delegation) under ADR-0007/ADR-0008.
--
-- End-state: 9 tables (8 domain tables per LLD §4 + the rls_violation_log
-- audit table), 8 enums, RLS on 7 tenant-scoped tables (FORCE ROW LEVEL
-- SECURITY + REVOKE ALL FROM PUBLIC + tenant_isolation policy),
-- record_version optimistic locking on 7 tables (all but processed_events).

-- ─────────────────────────────────────────────────────────────────────────
-- Extensions
-- ─────────────────────────────────────────────────────────────────────────
CREATE EXTENSION IF NOT EXISTS citext   WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;   -- gen_random_uuid()

-- ─────────────────────────────────────────────────────────────────────────
-- Enums (§4.1) — 8 types.
-- ─────────────────────────────────────────────────────────────────────────
-- 'member' is DERIVED-ONLY per TR-7 / §16 A29: never persisted in
-- tenant_roles; injected by I-8 into the effective role set. Kept in the
-- enum so both the wire and the derived value share a domain.
CREATE TYPE public.tenant_plan         AS ENUM ('starter', 'pro', 'enterprise');
CREATE TYPE public.subscription_status AS ENUM ('trial', 'active', 'past_due', 'cancelled', 'suspended', 'trial_expired', 'offboarded');
CREATE TYPE public.tenant_role         AS ENUM ('tenant_owner', 'tenant_admin', 'tender_admin', 'member');
CREATE TYPE public.membership_status   AS ENUM ('active', 'suspended', 'left');
CREATE TYPE public.dept_role           AS ENUM ('preparator', 'reviewer', 'approver');
CREATE TYPE public.realm_type          AS ENUM ('shared', 'dedicated');           -- §16 A22
CREATE TYPE public.invitation_status   AS ENUM ('pending', 'accepted', 'expired', 'revoked'); -- §16 A11
CREATE TYPE public.suspension_source   AS ENUM ('billing_lapse', 'operator');                 -- T-16, new — resolves RP-11

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
-- from within RLS policy checks. RLS is DISABLED on this table (see below)
-- so the logger cannot recurse into itself.
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
-- caller's transaction. Called from rls_check_tenant() below.
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
-- rls_check_tenant — STABLE STRICT SECURITY DEFINER. Returns true when the
-- row's tenant_id matches app.tenant_id GUC. Logs one of two violation
-- types on failure:
--   • missing_or_invalid_guc — no GUC set / malformed UUID (RLS-2)
--   • cross_tenant_access    — GUC present but points at a different tenant (RLS-3)
-- ─────────────────────────────────────────────────────────────────────────
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

-- ─────────────────────────────────────────────────────────────────────────
-- tenants (§4.2) — root aggregate. See T-1..T-16 for the full invariant
-- set. `plan` has no local FK (the plans catalog lives in the Catalog /
-- Admin Config Service) — validated app-side against an om:plans
-- read-through cache instead.
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
    cancelled_at             timestamptz,                                                          -- biconditional with status (T-11, §16 A24); NOT set for an operator-sourced suspension (T-16)
    suspension_source        public.suspension_source,                                             -- T-16, new — resolves RP-11; non-NULL iff status='suspended'
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
        CHECK (status IN ('trial', 'trial_expired', 'suspended') OR subscription_started_at IS NOT NULL), -- 'suspended' exempted by T-16 — an operator can suspend a never-converted trial tenant
    CONSTRAINT chk_offboarded_soft_deleted
        CHECK (status <> 'offboarded' OR deleted_at IS NOT NULL),                                    -- PAID-1
    CONSTRAINT chk_cancelled_at_required
        CHECK (
            CASE status
                WHEN 'cancelled'  THEN cancelled_at IS NOT NULL
                WHEN 'offboarded' THEN cancelled_at IS NOT NULL
                WHEN 'suspended'  THEN (suspension_source <> 'billing_lapse' OR cancelled_at IS NOT NULL)
                ELSE cancelled_at IS NULL
            END
        ),                                                                                           -- T-11, CASE form (T-16, new — resolves RP-11): the 'suspended' branch is conditional on suspension_source so an operator-sourced suspension never enters the grace/retention clock
    CONSTRAINT chk_suspension_source_required
        CHECK ((status = 'suspended') = (suspension_source IS NOT NULL))                             -- T-16, new — resolves RP-11; same lockstep discipline as chk_cancelled_at_required's original form
);

-- ─────────────────────────────────────────────────────────────────────────
-- tenant_departments (§4.2) — per-tenant activation of a department from
-- the Catalog Service's global catalog (external id, no local FK —
-- validated app-side against an om:departments read-through cache).
-- Composite PK (tenant_id, department_id) — no separate id column, TD-7
-- makes the pair the identity.
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
    CONSTRAINT tenant_departments_record_version_positive CHECK (record_version > 0)
);

-- ─────────────────────────────────────────────────────────────────────────
-- tenant_memberships (§4.2, §16 A14) — lifecycle-only, no role data.
-- The uq_tm_id_tenant_user unique index below is the composite FK target
-- for tenant_roles and dept_memberships (§16 A15/A28) — Tender ACL and
-- Delegation used to composite-FK to it too, before their tables moved to
-- separate databases (they now call this service's I-15 existence check
-- instead). MUST exist before those child tables.
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

-- Non-partial composite unique — REQUIRED as FK target for tenant_roles and
-- dept_memberships. Kept non-partial so composite FKs remain enforceable
-- even when deleted_at is set (a soft-deleted membership still pins its
-- child rows).
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
-- Composite FK to tenant_memberships (§16 A15/A28, DM-4) and to
-- tenant_departments (a user can only be assigned to a dept the tenant has
-- activated). No local FK on department_id itself — see tenant_departments.
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

-- ─────────────────────────────────────────────────────────────────────────
-- Indexes — partial indexes (WHERE deleted_at IS NULL / WHERE status='pending')
-- enable the "rejoin bug" fixes (TM-11, DM-3, PI-1) while keeping row
-- existence unique. Hot-path indexes annotated with the endpoint they serve.
-- ─────────────────────────────────────────────────────────────────────────
CREATE INDEX rls_violation_log_occurred_at_idx ON public.rls_violation_log (occurred_at);
CREATE INDEX idx_rls_violation_log_dedup       ON public.rls_violation_log (table_name, violation_type, occurred_at);

CREATE INDEX idx_tenants_status              ON public.tenants (status) WHERE deleted_at IS NULL;
CREATE INDEX idx_tenants_trial_end           ON public.tenants (trial_ends_at) WHERE status = 'trial';
-- T-6: realm reservation must persist through offboarding, so NO deleted_at
-- predicate on this partial unique — a soft-deleted dedicated realm still
-- reserves its name until the Keycloak realm itself is hard-deleted.
CREATE UNIQUE INDEX uq_tenants_realm_id_dedicated ON public.tenants (realm_id) WHERE realm_type = 'dedicated';
CREATE INDEX idx_tenants_ownerless           ON public.tenants (ownerless_since) WHERE ownerless_since IS NOT NULL;   -- T-13
CREATE INDEX idx_tenants_realm_sync_pending  ON public.tenants (updated_at)     WHERE realm_sync_pending;             -- T-15
CREATE INDEX idx_tenants_seat_overage        ON public.tenants (overage_since)  WHERE overage_since IS NOT NULL;      -- SEAT-5

CREATE INDEX idx_tenant_departments_tenant     ON public.tenant_departments (tenant_id)     WHERE is_active = true;
CREATE INDEX idx_tenant_departments_department ON public.tenant_departments (department_id) WHERE is_active = true;

CREATE UNIQUE INDEX uq_tm_active_user      ON public.tenant_memberships (tenant_id, user_id) WHERE deleted_at IS NULL;   -- TM-1/TM-11 rejoin
CREATE INDEX        idx_tm_tenant_id       ON public.tenant_memberships (tenant_id)          WHERE deleted_at IS NULL;
CREATE INDEX        idx_tm_user_id         ON public.tenant_memberships (user_id)            WHERE deleted_at IS NULL;
CREATE INDEX        idx_tm_status          ON public.tenant_memberships (tenant_id, status)  WHERE deleted_at IS NULL;
-- Keyset pagination seek key for P-4 (§21.2). Includes id as tiebreaker.
CREATE INDEX        idx_tm_tenant_created  ON public.tenant_memberships (tenant_id, created_at, id) WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX uq_tenant_roles_active     ON public.tenant_roles (tenant_id, user_id, role_code) WHERE deleted_at IS NULL;  -- TR-2
CREATE INDEX        idx_tenant_roles_tenant_user ON public.tenant_roles (tenant_id, user_id) WHERE deleted_at IS NULL;
CREATE INDEX        idx_tenant_roles_role      ON public.tenant_roles (tenant_id, role_code) WHERE deleted_at IS NULL;  -- TM-8 last-owner check
CREATE INDEX        idx_tenant_roles_membership ON public.tenant_roles (tenant_membership_id);

CREATE UNIQUE INDEX uq_dm_active_membership ON public.dept_memberships (tenant_id, user_id, department_id) WHERE deleted_at IS NULL;  -- DM-3
CREATE INDEX        idx_dm_tenant_user      ON public.dept_memberships (tenant_id, user_id) WHERE deleted_at IS NULL;
CREATE INDEX        idx_dm_tenant_dept      ON public.dept_memberships (tenant_id, department_id) WHERE deleted_at IS NULL;
CREATE INDEX        idx_dm_user_role        ON public.dept_memberships (user_id, role_level) WHERE deleted_at IS NULL;
-- AuthZ hot path (I-8): "who in this dept at this level?"
CREATE INDEX        idx_dm_dept_role        ON public.dept_memberships (tenant_id, department_id, role_level) WHERE deleted_at IS NULL;
CREATE INDEX        idx_dm_tenant_membership ON public.dept_memberships (tenant_membership_id);

CREATE INDEX idx_dept_role_labels_tenant ON public.dept_role_labels (tenant_id);

-- Rejoin-friendly partial unique: a terminal (accepted/expired/revoked)
-- invitation does NOT block a fresh 'pending' one for the same email.
CREATE UNIQUE INDEX uq_pi_pending    ON public.pending_invitations (tenant_id, email) WHERE status = 'pending';
CREATE INDEX idx_pi_tenant_pending  ON public.pending_invitations (tenant_id)         WHERE status = 'pending';   -- SEAT-1 pending count / P-30 list
CREATE INDEX idx_pi_expiry          ON public.pending_invitations (expires_at)        WHERE status = 'pending';   -- invitation-expiry CronJob
CREATE INDEX idx_pi_keycloak_user   ON public.pending_invitations (keycloak_user_id)  WHERE status = 'pending';   -- I-3 acceptance match
CREATE INDEX idx_pi_kc_cleanup      ON public.pending_invitations (id)                WHERE kc_cleanup_pending;   -- PI-9 reconciler

CREATE INDEX idx_processed_events_prune ON public.processed_events (processed_at);

-- ─────────────────────────────────────────────────────────────────────────
-- Triggers — touch_row() on every record_version-carrying table (TRG-1..3,
-- 7 tables total). WHEN (OLD.* IS DISTINCT FROM NEW.*) so a no-op UPDATE
-- does not bump record_version or updated_at (TRG-3). processed_events is
-- deliberately excluded — it has no record_version.
-- ─────────────────────────────────────────────────────────────────────────
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
-- pending_invitations.expires_at defense-in-depth (PI-1). The service layer
-- already enforces `expires_at > now()` on invitation create, but a direct
-- DB write could stage a past-expiry pending row that instantly stops
-- holding a seat without any lifecycle transition. UPDATE is intentionally
-- NOT gated — the accept/revoke/expiry flows legitimately transition rows
-- after their expires_at has passed. LLD §16 A11 / PI-1, §17 `invalid_expires_at`.
-- ─────────────────────────────────────────────────────────────────────────
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

-- ─────────────────────────────────────────────────────────────────────────
-- Row-Level Security (§4.3, RLS-1..RLS-6) — second of three isolation
-- layers (§10.1). ENABLE + FORCE means even the table owner cannot bypass
-- RLS without an explicit BYPASSRLS role. WITH CHECK gates writes, USING
-- gates reads; both invoke rls_check_tenant() so violations get
-- sampled-logged.
--
-- The tenants policy compares against `id` (not `tenant_id`) because
-- tenants.id IS the tenant PK — single-row visibility per tenant, routed
-- through rls_check_tenant() like every other table so cross-tenant
-- access on the root table is sampled-logged too, not just a raw ::uuid
-- cast error on missing GUC.
--
-- rls_violation_log stays RLS-disabled (recursion guard — see the comment
-- on log_rls_violation() above).
-- ─────────────────────────────────────────────────────────────────────────
ALTER TABLE public.tenants             ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenants             FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.tenant_departments  ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_departments  FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.tenant_memberships  ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_memberships  FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.tenant_roles        ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.tenant_roles        FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.dept_memberships    ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.dept_memberships    FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.dept_role_labels    ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.dept_role_labels    FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.pending_invitations ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.pending_invitations FORCE  ROW LEVEL SECURITY;

-- Explicitly REVOKE from PUBLIC so a mis-provisioned role cannot silently
-- read tenant tables without going through the policy (§4.3, defense-in-depth).
REVOKE ALL ON
    public.tenants,
    public.tenant_departments,
    public.tenant_memberships,
    public.tenant_roles,
    public.dept_memberships,
    public.dept_role_labels,
    public.pending_invitations
FROM PUBLIC;

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

ALTER TABLE public.rls_violation_log DISABLE ROW LEVEL SECURITY;
ALTER TABLE public.rls_violation_log NO FORCE ROW LEVEL SECURITY;

-- ─────────────────────────────────────────────────────────────────────────
-- Roles & grants (§4.3 / §4.4 / RLS-4 / MIG-3).
--
-- Three DB roles in play:
--   • org_membership_app       — runtime app. Does NOT hold BYPASSRLS (RLS-4).
--   • org_membership_migrator  — migrations + reconciler. Holds BYPASSRLS.
--   • admin_readonly           — cross-tenant compliance/support SELECTs.
--                                NOLOGIN + BYPASSRLS. Grants SELECT only.
--
-- In production the roles are provisioned by Terraform (owns the CREATE
-- ROLE step outside migrations); this migration then attaches the correct
-- grants and idempotently re-asserts BYPASSRLS. In dev the migrator user
-- typically lacks CREATEROLE, so we skip creation gracefully and emit a
-- NOTICE. Idempotent + safe to re-run in every environment.
-- ─────────────────────────────────────────────────────────────────────────
DO $$
DECLARE
    v_can_create boolean;
BEGIN
    -- ── admin_readonly ────────────────────────────────────────────────────
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        BEGIN
            ALTER ROLE admin_readonly BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'admin_readonly exists but current user cannot ALTER ROLE — skipping BYPASSRLS refresh (fine in dev).';
        END;
    ELSE
        SELECT rolcreaterole OR rolsuper
          INTO v_can_create
          FROM pg_roles
         WHERE rolname = current_user;

        IF v_can_create THEN
            CREATE ROLE admin_readonly NOLOGIN BYPASSRLS;
        ELSE
            RAISE NOTICE 'admin_readonly role missing and current user (%) lacks CREATEROLE — skipping role creation. In production this role is provisioned by Terraform per LLD §4.3.', current_user;
        END IF;
    END IF;

    -- Grants — only run if the role exists at this point.
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        BEGIN
            REVOKE ALL ON
                public.tenants,
                public.tenant_departments,
                public.tenant_memberships,
                public.tenant_roles,
                public.dept_memberships,
                public.dept_role_labels,
                public.pending_invitations
            FROM admin_readonly;

            GRANT SELECT ON
                public.tenants,
                public.tenant_departments,
                public.tenant_memberships,
                public.tenant_roles,
                public.dept_memberships,
                public.dept_role_labels,
                public.pending_invitations
            TO admin_readonly;

            -- Housekeeping tables — useful for compliance reads, but no writes.
            GRANT SELECT ON public.rls_violation_log, public.processed_events
                TO admin_readonly;

            EXECUTE $c$COMMENT ON ROLE admin_readonly IS
                'LLD §4.3 cross-tenant compliance/support reader. BYPASSRLS + SELECT only.'$c$;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'Grants to admin_readonly skipped — current user (%) lacks the GRANT privilege on one or more target tables. In production the migrator role has this.', current_user;
        END;
    END IF;

    -- ── org_membership_migrator BYPASSRLS assertion ────────────────────────
    -- The scripts/init-db.sql local dev bootstrap creates this role with
    -- BYPASSRLS. In production Terraform owns creation; this block idempotently
    -- re-asserts the flag in case it was ever revoked.
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator') THEN
        BEGIN
            ALTER ROLE org_membership_migrator BYPASSRLS;
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE NOTICE 'org_membership_migrator exists but current user cannot ALTER ROLE — skipping BYPASSRLS refresh.';
        END;
    END IF;

    -- ── org_membership_app defensive check ─────────────────────────────────
    -- If org_membership_app somehow acquired BYPASSRLS (misconfiguration),
    -- strip it here. RLS-4 requires this role to be RLS-scoped.
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app' AND rolbypassrls) THEN
        BEGIN
            ALTER ROLE org_membership_app NOBYPASSRLS;
            RAISE NOTICE 'Stripped BYPASSRLS from org_membership_app (RLS-4 invariant restored).';
        EXCEPTION WHEN insufficient_privilege THEN
            RAISE WARNING 'org_membership_app has BYPASSRLS but current user cannot ALTER ROLE — RLS-4 VIOLATION requires operator action.';
        END;
    END IF;
END$$;

-- ─────────────────────────────────────────────────────────────────────────
-- Grants the runtime `org_membership_app` role the table-level privileges
-- it needs to reach the RLS policy layer (RLS-4/MIG-3). A policy can only
-- apply to a role that already has table-level DML — otherwise every
-- SELECT/INSERT fails at the privilege check before RLS is consulted.
--
-- In local dev this is masked because `POSTGRES_USER=org_membership_app`
-- starts as the DB owner (init-db.sql), so grants are implicit. In staging
-- and production `org_membership_app` is a distinct low-privilege role
-- provisioned by Terraform; without this migration every query fails
-- permission denied at first cutover. Idempotent: GRANT is additive and
-- safe to re-run.
--
-- The `rls_violation_log` table is written to via `log_rls_violation()`,
-- which is called by `rls_check_tenant()` — SECURITY DEFINER — so the app
-- role does not need a direct grant on that table.
-- ─────────────────────────────────────────────────────────────────────────
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
        RAISE NOTICE 'org_membership_app role missing — skipping grants. In production Terraform provisions this role before migrations run.';
        RETURN;
    END IF;

    BEGIN
        -- ── Tenant-scoped tables (RLS-3 gates the writes) ─────────────────
        GRANT SELECT, INSERT, UPDATE, DELETE ON
            public.tenants,
            public.tenant_departments,
            public.tenant_memberships,
            public.tenant_roles,
            public.dept_memberships,
            public.dept_role_labels,
            public.pending_invitations
        TO org_membership_app;

        -- ── Housekeeping tables ───────────────────────────────────────────
        -- processed_events: consumer inserts on every applied event; the
        -- processed-events-prune cron deletes rows older than 8 days (PE-1).
        GRANT SELECT, INSERT, DELETE ON public.processed_events TO org_membership_app;

        -- outbox_events: created by platform-events; publisher inserts in
        -- every business tx (EVT-10); the outbox runner reads/marks/prunes.
        -- Guarded IF EXISTS because platform-events owns the migration and
        -- may not have run yet in an out-of-order dev bootstrap.
        IF EXISTS (SELECT 1 FROM information_schema.tables
                   WHERE table_schema = 'public' AND table_name = 'outbox_events') THEN
            GRANT SELECT, INSERT, UPDATE, DELETE ON public.outbox_events TO org_membership_app;
        END IF;
        IF EXISTS (SELECT 1 FROM information_schema.tables
                   WHERE table_schema = 'public' AND table_name = 'outbox_dead_letters') THEN
            GRANT SELECT, INSERT, UPDATE, DELETE ON public.outbox_dead_letters TO org_membership_app;
        END IF;

        -- ── Sequences on any of the above (uuid PKs use gen_random_uuid()
        -- rather than sequences, so no ALL SEQUENCES grant is needed today;
        -- keep this comment so a future serial-PK column adds an explicit
        -- GRANT rather than reaching for a blanket).
    EXCEPTION WHEN insufficient_privilege THEN
        RAISE NOTICE 'Grants to org_membership_app skipped — current user (%) lacks GRANT privilege. In production the migrator role has this; in dev the app role is the owner.', current_user;
    END;
END$$;

-- ─────────────────────────────────────────────────────────────────────────
-- outbox_events customization — platform-events creates outbox_events with
-- a JSONB payload column (via outbox.ApplySchema, which cmd/server/main.go
-- runs BEFORE this domain migration). pgx encodes []byte as bytea hex in
-- PgBouncer SimpleProtocol mode, which is invalid for jsonb; text accepts
-- the raw bytes as-is, and the outbox runner reads the value back via JSON
-- unmarshal, which works identically with text.
-- ─────────────────────────────────────────────────────────────────────────
ALTER TABLE outbox_events ALTER COLUMN payload TYPE text USING payload::text;

-- pgx SimpleProtocol encodes []byte as bytea hex (\x...) even for text
-- columns. This trigger decodes it back to UTF-8 text on every INSERT so
-- the outbox runner can JSON-unmarshal the payload without errors.
CREATE OR REPLACE FUNCTION outbox_normalize_payload()
RETURNS trigger AS $$
BEGIN
  IF left(NEW.payload, 2) = '\x' THEN
    NEW.payload = convert_from(decode(substring(NEW.payload FROM 3), 'hex'), 'UTF8');
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_outbox_normalize_payload
BEFORE INSERT ON outbox_events
FOR EACH ROW EXECUTE FUNCTION outbox_normalize_payload();
