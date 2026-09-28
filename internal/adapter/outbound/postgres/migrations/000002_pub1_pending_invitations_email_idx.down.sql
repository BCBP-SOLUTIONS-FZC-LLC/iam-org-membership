-- Rollback for 000002_pub1_pending_invitations_email_idx.
DROP INDEX IF EXISTS public.idx_pi_email_pending;
