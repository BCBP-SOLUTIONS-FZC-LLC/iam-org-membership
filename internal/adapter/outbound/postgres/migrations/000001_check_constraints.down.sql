-- Rollback for 000001_check_constraints.
ALTER TABLE public.dept_role_labels      DROP CONSTRAINT IF EXISTS chk_display_name_length;
ALTER TABLE public.pending_invitations   DROP CONSTRAINT IF EXISTS chk_full_name_length;
