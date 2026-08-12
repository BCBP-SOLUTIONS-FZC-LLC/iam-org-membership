-- §16 A71, DEL-14: per-tenant delegation governance columns.
-- delegation_max_duration_days: caps fixed-end delegation span (ends_at - starts_at).
-- delegation_review_window_days: tenant-configurable default review cycle for open-ended
--   delegations (DEL-13); supersedes the global DELEGATION_REVIEW_WINDOW_DAYS env var.
-- Both follow the exact mfa_freshness_seconds pattern (§16 A20): NOT NULL DEFAULT 90,
-- CHECK BETWEEN 1 AND 180. Zero-downtime additive migration (constant DEFAULT).

ALTER TABLE tenants
    ADD COLUMN delegation_max_duration_days  int NOT NULL DEFAULT 90,
    ADD COLUMN delegation_review_window_days int NOT NULL DEFAULT 90;

ALTER TABLE tenants
    ADD CONSTRAINT tenants_delegation_max_duration_range   CHECK (delegation_max_duration_days  BETWEEN 1 AND 180),
    ADD CONSTRAINT tenants_delegation_review_window_range  CHECK (delegation_review_window_days BETWEEN 1 AND 180);
