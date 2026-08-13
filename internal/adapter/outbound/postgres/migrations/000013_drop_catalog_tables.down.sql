-- Best-effort restore of 000013_drop_catalog_tables.up.sql — schema + seed
-- rows only. Does NOT and CANNOT restore any tenant_departments/
-- dept_memberships/group_dept_mappings rows that referenced the original
-- (dropped) department UUIDs, since the INSERTs below generate fresh ones.
-- Safe for migration round-trip testing against an empty/fresh database;
-- NOT a substitute for a pre-drop snapshot on a database with real data.

CREATE TABLE public.plans (
    code                    public.tenant_plan     NOT NULL,
    display_name            text                   NOT NULL,
    workflow_template_limit int,
    tender_limit            int,
    trial_duration_days     int                    NOT NULL,
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

INSERT INTO public.plans (code, display_name, workflow_template_limit, tender_limit, trial_duration_days, sso_enabled, custom_branding, feature_set) VALUES
    ('starter',    'Starter',     5,    10,   30, false, 'none', '{}'::jsonb),
    ('pro',        'Pro',         50,   100,  30, false, 'logo', '{}'::jsonb),
    ('enterprise', 'Enterprise',  NULL, NULL, 30, true,  'logo', '{"require_mfa_all_users_allowed": true}'::jsonb)
ON CONFLICT (code) DO NOTHING;

CREATE TABLE public.departments (
    id             uuid        NOT NULL DEFAULT gen_random_uuid(),
    code           text        NOT NULL,
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

INSERT INTO public.departments (code, name, is_system) VALUES
    ('ENGINEERING', 'Engineering', true),
    ('DESIGN',      'Design',      true),
    ('PROCUREMENT', 'Procurement', true),
    ('FINANCE',     'Finance',     true),
    ('LEGAL',       'Legal',       true)
ON CONFLICT (code) DO NOTHING;

ALTER TABLE public.tenants
    ADD CONSTRAINT fk_tenants_plan FOREIGN KEY (plan) REFERENCES public.plans(code);

ALTER TABLE public.tenant_departments
    ADD CONSTRAINT fk_td_department FOREIGN KEY (department_id) REFERENCES public.departments(id) ON DELETE RESTRICT;

ALTER TABLE public.dept_memberships
    ADD CONSTRAINT fk_dm_department FOREIGN KEY (department_id) REFERENCES public.departments(id) ON DELETE RESTRICT;

ALTER TABLE public.group_dept_mappings
    ADD CONSTRAINT fk_gdm_department FOREIGN KEY (department_id) REFERENCES public.departments(id) ON DELETE RESTRICT;
