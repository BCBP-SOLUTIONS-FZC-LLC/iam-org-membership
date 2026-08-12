-- Phase 2 · Down migration for 000007_pending_invitations_expiry_guard.

DROP TRIGGER IF EXISTS trg_pending_invitations_expiry_guard ON public.pending_invitations;
DROP FUNCTION IF EXISTS public.pending_invitations_expires_at_check();
