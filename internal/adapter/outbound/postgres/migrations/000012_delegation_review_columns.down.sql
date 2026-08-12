DROP INDEX CONCURRENTLY IF EXISTS idx_delegations_review_due;

ALTER TABLE delegations
    DROP COLUMN IF EXISTS review_window_days,
    DROP COLUMN IF EXISTS review_notice_sent_at,
    DROP COLUMN IF EXISTS review_due_at;
