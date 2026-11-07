-- Dead letters become pending messages again, which the relay retries.
DROP INDEX IF EXISTS outbox_events_failed;
DROP INDEX IF EXISTS outbox_events_pending;
ALTER TABLE outbox_events DROP COLUMN IF EXISTS failed_at;
CREATE INDEX outbox_events_pending ON outbox_events (id) WHERE published_at IS NULL;
