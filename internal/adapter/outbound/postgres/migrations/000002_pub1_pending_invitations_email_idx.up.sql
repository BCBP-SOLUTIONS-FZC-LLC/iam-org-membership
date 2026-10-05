-- PUB-1 (LLD §5.4, IAM HLD §8.2.7): the pre-login tenant/realm lookup
-- matches a pending invitation by email alone. uq_pi_pending is
-- (tenant_id, email), which can't serve an email-only equality lookup, so
-- this partial index backs that half of ResolveByIdentifier. The slug half
-- is already served by uq_tenants_slug. Additive only (MIG-1).
CREATE INDEX IF NOT EXISTS idx_pi_email_pending
    ON public.pending_invitations (email)
    WHERE status = 'pending';
