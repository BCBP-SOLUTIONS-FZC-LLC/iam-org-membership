-- 000010 rollback
ALTER TABLE public.tenants DROP COLUMN IF EXISTS seats_last_event_at;
