-- Phase 2 · Migration 000007 — PI-1 defense-in-depth
--
-- The service-layer already enforces `expires_at > now()` on invitation
-- create, but a direct DB write (or a mis-sequenced migration backfill)
-- could stage a past-expiry pending row that instantly stops holding a seat
-- without any lifecycle transition. This BEFORE INSERT trigger closes the
-- gap. UPDATE is intentionally NOT gated — the accept/revoke/expiry flows
-- legitimately transition rows after their expires_at has passed.
--
-- LLD §16 A11 / PI-1, §17 `invalid_expires_at`.

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
