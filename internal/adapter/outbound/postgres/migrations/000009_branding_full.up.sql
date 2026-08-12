-- Add 'full' to branding_level enum to match DTO and LLD §16 A19.
-- ALTER TYPE ADD VALUE cannot run inside a transaction block.
ALTER TYPE public.branding_level ADD VALUE IF NOT EXISTS 'full';
