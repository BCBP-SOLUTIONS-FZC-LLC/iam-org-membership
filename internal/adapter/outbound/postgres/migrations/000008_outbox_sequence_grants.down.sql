-- Reverses 000008 for exactly the roles it granted: those that do not own
-- outbox_events_ordering_seq. An owner is never touched, so a rollback run
-- by a superuser cannot strip the owner's own privileges. REVOKE removes
-- only grants made by the current user, so grants from Terraform survive.

DO $$
DECLARE
    seq_owner name;
BEGIN
    SELECT pg_get_userbyid(c.relowner) INTO seq_owner
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'outbox_events_ordering_seq' AND c.relkind = 'S';

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_app') THEN
        IF current_user <> 'org_membership_app' THEN
            ALTER DEFAULT PRIVILEGES IN SCHEMA public
                REVOKE USAGE, SELECT ON SEQUENCES FROM org_membership_app;
        END IF;
        IF seq_owner IS NOT NULL AND seq_owner <> 'org_membership_app' THEN
            REVOKE USAGE, SELECT ON SEQUENCE public.outbox_events_ordering_seq FROM org_membership_app;
        END IF;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'org_membership_migrator') THEN
        IF current_user <> 'org_membership_migrator' THEN
            ALTER DEFAULT PRIVILEGES IN SCHEMA public
                REVOKE USAGE, SELECT ON SEQUENCES FROM org_membership_migrator;
        END IF;
        IF seq_owner IS NOT NULL AND seq_owner <> 'org_membership_migrator' THEN
            REVOKE USAGE, SELECT ON SEQUENCE public.outbox_events_ordering_seq FROM org_membership_migrator;
        END IF;
    END IF;
END
$$;
