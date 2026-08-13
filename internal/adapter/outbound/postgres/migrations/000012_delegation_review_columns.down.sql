-- Plain DROP INDEX (not CONCURRENTLY) — same rationale as the up migration:
-- golang-migrate runs every migration inside a transaction, and
-- CONCURRENTLY cannot run inside one (SQLSTATE 25001).
DROP INDEX IF EXISTS idx_delegations_review_due;

ALTER TABLE delegations
    DROP COLUMN IF EXISTS review_window_days,
    DROP COLUMN IF EXISTS review_notice_sent_at,
    DROP COLUMN IF EXISTS review_due_at;
