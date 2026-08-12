-- §16 A70, DEL-13: delegation review-window columns.
-- review_due_at: set at creation to starts_at + delegation_review_window_days for open-ended
--   delegations (ends_at IS NULL). NULL for fixed-end delegations (they expire on their own).
-- review_notice_sent_at: tracks whether the current cycle's warning has been sent.
--   Reset to NULL whenever review_due_at is pushed forward (P-32 extend).
-- review_window_days: optional per-delegation override of the tenant's default review cycle.
-- Partial index mirrors idx_delegations_ends_at pattern for CronJob efficiency.
-- Zero-downtime additive migration (all nullable, no backfill required).

ALTER TABLE delegations
    ADD COLUMN review_due_at        timestamptz,
    ADD COLUMN review_notice_sent_at timestamptz,
    ADD COLUMN review_window_days   int;

CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_delegations_review_due
    ON delegations (review_due_at)
    WHERE ends_at IS NULL AND status = 'active';
