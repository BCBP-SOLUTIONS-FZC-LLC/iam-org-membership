-- Change outbox_events.payload from jsonb to text.
-- platform-events outboxstore.InsertRecord passes []byte which pgx encodes as
-- bytea hex in PgBouncer SimpleProtocol mode — invalid for jsonb. text accepts
-- the raw bytes as-is; the outbox runner reads the value back via JSON
-- unmarshal which works identically with text.
ALTER TABLE outbox_events ALTER COLUMN payload TYPE text USING payload::text;

-- pgx SimpleProtocol encodes []byte as bytea hex (\x...) even for text columns.
-- This trigger decodes it back to UTF-8 text on every INSERT so the outbox
-- runner can JSON-unmarshal the payload without errors.
CREATE OR REPLACE FUNCTION outbox_normalize_payload()
RETURNS trigger AS $$
BEGIN
  IF left(NEW.payload, 2) = '\x' THEN
    NEW.payload = convert_from(decode(substring(NEW.payload FROM 3), 'hex'), 'UTF8');
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_outbox_normalize_payload
BEFORE INSERT ON outbox_events
FOR EACH ROW EXECUTE FUNCTION outbox_normalize_payload();
