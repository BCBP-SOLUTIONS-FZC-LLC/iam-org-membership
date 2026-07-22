-- Phase 0 bootstrap. Intentionally empty — Phase 1 lands the 18 initial
-- migrations (extensions, enums, plans catalog, departments, tenants, ...)
-- per LLD §19.5 hard-ordering.
--
-- This file exists so //go:embed migrations/*.sql matches at least one entry
-- at build time. Do NOT delete without also deleting the down pair — the
-- pgmigrate runner requires paired up/down files.
SELECT 1;
