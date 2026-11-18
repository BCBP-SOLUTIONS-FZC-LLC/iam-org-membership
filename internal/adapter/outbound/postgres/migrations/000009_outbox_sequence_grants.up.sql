-- 000009_outbox_sequence_grants — the runtime roles can draw from
-- platform-events' outbox_events_ordering_seq.
--
-- platform-events migration 010 (v2.2.x) added the sequence; every keyed
-- enqueue (eventbus.Publisher → outbox.EnqueueOrdered) calls nextval() on it
-- inside the business transaction. 000000 granted the outbox TABLES only,
-- and both 000000 and 000007 state that no sequence grants are needed —
-- wrong since 010. A non-owner role therefore failed every keyed enqueue
-- with "permission denied for sequence outbox_events_ordering_seq"
-- (SQLSTATE 42501), returning 500 from I-1 signup and every other path that
-- emits a keyed event.
--
--   • org_membership_app      — the request pool (always a non-owner outside
--                               local dev).
--   • org_membership_migrator — the sysPool also writes the outbox
--                               (reconcilers, trial-cleanup); it needs the
--                               grant only when it is not the owner (MIG-10).
--
-- USAGE is what nextval() requires; SELECT allows currval()/reads. No UPDATE
-- (setval) — nothing in the runtime resets the sequence.
--
-- Default privileges give sequences the migrating role creates later the
-- same USAGE, SELECT for both roles, so a future platform-events sequence
-- cannot reintroduce the gap.
--
-- Guarded IF EXISTS on the sequence only for an out-of-order dev bootstrap
-- (cmd/migrate applies the platform-events schema first, so it exists in
-- every deployed environment). No exception handler: a grant the running
-- role cannot make fails the migration loudly instead of recording it as
-- applied with keyed enqueues still broken. Additive only (MIG-1).

DO $$
DECLARE
    seq_owner name;
BEGIN
    SELECT pg_get_userbyid(c.relowner) INTO seq_owner
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relname = 'outbox_events_ordering_seq' AND c.relkind = 'S';

    IF seq_owner IS NULL THEN
        RAISE NOTICE '000009: outbox_events_ordering_seq does not exist — platform-events schema not applied yet; skipping sequence grant.';
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
        RAISE NOTICE '000009: org_membership_app does not exist — nothing to grant.';
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
