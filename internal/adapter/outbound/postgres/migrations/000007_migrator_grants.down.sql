-- Reverses 000007 on exactly the tables it granted: those the migrator does
-- not own. A table the migrator owns is never touched, so a rollback run by
-- a superuser cannot strip the owner's own privileges. REVOKE removes only
-- grants made by the current user, so grants from Terraform survive.

DO $$
DECLARE
    t record;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator') THEN
        RETURN;
    END IF;
    IF current_user <> 'org_membership_migrator' THEN
        ALTER DEFAULT PRIVILEGES IN SCHEMA public
            REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM org_membership_migrator;
    END IF;
    FOR t IN
        SELECT tablename FROM pg_tables
        WHERE schemaname = 'public'
          AND tableowner <> 'org_membership_migrator'
          AND tablename <> 'schema_migrations'
    LOOP
        EXECUTE format('REVOKE SELECT, INSERT, UPDATE, DELETE ON public.%I FROM org_membership_migrator', t.tablename);
    END LOOP;
END
$$;
