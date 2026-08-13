DROP TRIGGER IF EXISTS trg_outbox_normalize_payload ON outbox_events;
DROP FUNCTION IF EXISTS outbox_normalize_payload();

ALTER TABLE outbox_events ALTER COLUMN payload TYPE jsonb USING payload::jsonb;
