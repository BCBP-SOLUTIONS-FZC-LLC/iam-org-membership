ALTER TABLE tenants
    DROP CONSTRAINT IF EXISTS tenants_delegation_max_duration_range,
    DROP CONSTRAINT IF EXISTS tenants_delegation_review_window_range;

ALTER TABLE tenants
    DROP COLUMN IF EXISTS delegation_max_duration_days,
    DROP COLUMN IF EXISTS delegation_review_window_days;
