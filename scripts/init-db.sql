-- Local dev bootstrap for the org_membership database.
--
-- Creates the two DB roles the LLD calls out (§4.3, RLS-4, MIG-3/5):
--   org_membership_app       — the runtime app role. RLS enforced (NO BYPASSRLS).
--   org_membership_migrator  — the migration + reconciler role. HAS BYPASSRLS.
--
-- It runs as the compose superuser (POSTGRES_USER=postgres). Neither role is
-- a superuser: a superuser bypasses RLS, so an app connecting as one would
-- never exercise the tenant policies.

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
    CREATE ROLE org_membership_app LOGIN PASSWORD 'devpassword' NOSUPERUSER NOBYPASSRLS;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator') THEN
    -- BYPASSRLS is only meaningful when the connection actually authenticates
    -- as this role. RLS still applies to org_membership_app connections.
    CREATE ROLE org_membership_migrator LOGIN PASSWORD 'devpassword' BYPASSRLS;
    GRANT ALL PRIVILEGES ON DATABASE org_membership TO org_membership_migrator;
  END IF;
END
$$;

-- Extensions — declared here so a fresh dev DB has them before migrations
-- run. Migrations idempotently re-declare via CREATE EXTENSION IF NOT EXISTS.
CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Baseline privileges. The app role owns nothing at first — migrations run
-- as org_membership_migrator (via MIGRATION_DATABASE_URL), which owns what
-- they create and GRANTs specific privileges to org_membership_app inside
-- each migration. Postgres 15+ no longer lets PUBLIC create in schema
-- public, so the migrator needs CREATE there explicitly.
GRANT CONNECT ON DATABASE org_membership TO org_membership_app;
GRANT USAGE ON SCHEMA public TO org_membership_app;
GRANT USAGE, CREATE ON SCHEMA public TO org_membership_migrator;
