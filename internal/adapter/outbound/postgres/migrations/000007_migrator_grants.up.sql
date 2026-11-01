-- 000007_migrator_grants — the BYPASSRLS sysPool role can use the tables.
--
-- org_membership_migrator backs the sysPool (SYSTEM_DATABASE_URL): PUB-1's
-- resolve, I-16, the reconciler CronJobs and the business-metric exporters.
-- Earlier migrations granted the app role and admin_readonly but never the
-- migrator, assuming it owns every table because it runs the migrations.
-- When another role runs them (the local compose migrated as
-- org_membership_app; an RDS master user would too), the migrator owns
-- nothing: BYPASSRLS skips policies, not privileges, so PUB-1 answered 500
-- (permission denied) and every cross-tenant sweep failed (MIG-10).
--
-- Per table, only where the migrator is not the owner (an owner needs no
-- grant, and a REVOKE in the down must never reach an owner):
--   • DML (SELECT, INSERT, UPDATE, DELETE): the reconcilers update and
--     delete, the prune jobs delete, the outbox is written.
--   • rls_violation_log: SELECT only. Only the SECURITY DEFINER
--     log_rls_violation() writes it, and the sysPool must not be able to
--     rewrite the RLS audit trail.
--   • schema_migrations: nothing.
-- No sequence grants: the only sequence belongs to rls_violation_log.
--
-- Default privileges give tables the migrating role creates later the same
-- DML, so a new table cannot reintroduce the gap; a migration that needs
-- less (another audit-style table) revokes it explicitly.
--
-- No exception handler: a grant the running role cannot make fails the
-- migration loudly instead of recording it as applied with PUB-1 still
-- broken. Additive only (MIG-1).

DO $$
DECLARE
    t record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator') THEN
        RAISE NOTICE '000007: org_membership_migrator does not exist — nothing to grant.';
        RETURN;
    END IF;
    FOR t IN
        SELECT tablename FROM pg_tables
        WHERE schemaname = 'public'
          AND tableowner <> 'org_membership_migrator'
          AND tablename <> 'schema_migrations'
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
