-- 000001_check_constraints — additive constraints only; safe to run on an
-- empty or populated database. Forward-only per MIG-1.
--
-- SECURITY-2: cap pending_invitations.full_name to 512 chars so the column
-- matches the application-layer validation already enforced in
-- invitation_service.go (Invite: "full_name must not exceed 512 characters").
-- A DB-level check prevents a bypass via direct SQL or any future code path
-- that skips the service layer.
ALTER TABLE public.pending_invitations
    ADD CONSTRAINT chk_full_name_length CHECK (length(full_name) <= 512);

-- SECURITY-9: cap dept_role_labels.display_name to 255 chars. There is no
-- existing application-layer length validation for this field, so the DB
-- constraint is the sole enforcement point until a service-layer check is
-- added.
ALTER TABLE public.dept_role_labels
    ADD CONSTRAINT chk_display_name_length CHECK (length(display_name) <= 255);
