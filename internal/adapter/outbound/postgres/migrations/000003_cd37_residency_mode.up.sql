-- 000003_cd37_residency_mode — CD-37 data-residency column (TL decision 2026-10-15).
--
-- Adds tenants.residency_mode: available on ALL plans, settable only by
-- tenant_owner via PATCH /api/v1/tenants/:id (P-2), injected by AuthZ as
-- x-residency-mode. Default 'standard' so all existing rows are non-breaking.
--
-- Path taken: new migration file (not an in-place edit of 000000) to stay
-- consistent with the 000001/000002 additive pattern already in use.

ALTER TABLE public.tenants
    ADD COLUMN residency_mode text NOT NULL DEFAULT 'standard'
        CONSTRAINT tenants_residency_mode_check CHECK (residency_mode IN ('standard', 'strict'));
