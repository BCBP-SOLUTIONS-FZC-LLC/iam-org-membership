-- Best-effort restore of 000014_drop_group_mapping_tables.up.sql — schema
-- only, no data. Safe for migration round-trip testing against an empty/
-- fresh database; NOT a substitute for a pre-drop snapshot on a database
-- with real rows, per the LLD §18 Stage 4 rollback note. fk_gdm_department
-- is intentionally NOT recreated — departments moved to the Catalog
-- service and that FK was already dropped in 000013.

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
    CONSTRAINT chk_gtrm_no_member            CHECK (role_code <> 'member'),
    CONSTRAINT gtrm_group_name_not_empty     CHECK (keycloak_group_name <> ''),
    CONSTRAINT gtrm_record_version_positive  CHECK (record_version > 0)
);

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
    CONSTRAINT gdm_group_name_not_empty    CHECK (keycloak_group_name <> ''),
    CONSTRAINT gdm_record_version_positive CHECK (record_version > 0)
);

CREATE INDEX idx_gdrm_tenant ON public.group_dept_role_mappings   (tenant_id);
CREATE INDEX idx_gtrm_tenant ON public.group_tenant_role_mappings (tenant_id);
CREATE INDEX idx_gdm_tenant  ON public.group_dept_mappings        (tenant_id);
CREATE INDEX idx_gdm_group   ON public.group_dept_mappings        (tenant_id, keycloak_group_name);

CREATE TRIGGER trg_touch_group_dept_role_mappings
    BEFORE UPDATE ON public.group_dept_role_mappings
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_group_tenant_role_mappings
    BEFORE UPDATE ON public.group_tenant_role_mappings
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

CREATE TRIGGER trg_touch_group_dept_mappings
    BEFORE UPDATE ON public.group_dept_mappings
    FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*)
    EXECUTE FUNCTION public.touch_row();

ALTER TABLE public.group_dept_role_mappings   ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.group_dept_role_mappings   FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.group_tenant_role_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.group_tenant_role_mappings FORCE  ROW LEVEL SECURITY;
ALTER TABLE public.group_dept_mappings        ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.group_dept_mappings        FORCE  ROW LEVEL SECURITY;

REVOKE ALL ON
    public.group_dept_role_mappings,
    public.group_tenant_role_mappings,
    public.group_dept_mappings
FROM PUBLIC;

CREATE POLICY tenant_isolation ON public.group_dept_role_mappings
    USING      (rls_check_tenant(tenant_id, 'group_dept_role_mappings'))
    WITH CHECK (rls_check_tenant(tenant_id, 'group_dept_role_mappings'));

CREATE POLICY tenant_isolation ON public.group_tenant_role_mappings
    USING      (rls_check_tenant(tenant_id, 'group_tenant_role_mappings'))
    WITH CHECK (rls_check_tenant(tenant_id, 'group_tenant_role_mappings'));

CREATE POLICY tenant_isolation ON public.group_dept_mappings
    USING      (rls_check_tenant(tenant_id, 'group_dept_mappings'))
    WITH CHECK (rls_check_tenant(tenant_id, 'group_dept_mappings'));

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON
            public.group_dept_role_mappings,
            public.group_tenant_role_mappings,
            public.group_dept_mappings
        TO org_membership_app;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
        GRANT SELECT ON
            public.group_dept_role_mappings,
            public.group_tenant_role_mappings,
            public.group_dept_mappings
        TO admin_readonly;
    END IF;
END $$;
