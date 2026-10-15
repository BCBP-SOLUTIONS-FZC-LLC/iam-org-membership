-- 000003_cd37_residency_mode DOWN — removes the residency_mode column.
ALTER TABLE public.tenants DROP COLUMN IF EXISTS residency_mode;
