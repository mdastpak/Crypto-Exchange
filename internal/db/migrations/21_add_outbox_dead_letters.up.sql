-- Dead letters of the outbox (#192). A message that still fails after the
-- attempts its topic allows is set aside with failed_at, so that the later
-- messages of its topic are published instead of waiting forever. The
-- admin outbox commands list, retry, and discard such messages.
ALTER TABLE outbox_events ADD COLUMN failed_at TIMESTAMPTZ;

-- The relay reads the pending messages of each topic in id order, so that
-- one topic with many pending messages does not hide the others.
DROP INDEX IF EXISTS outbox_events_pending;
CREATE INDEX outbox_events_pending ON outbox_events (topic, id)
    WHERE published_at IS NULL AND failed_at IS NULL;
-- Dead letters are few and are listed by id.
CREATE INDEX outbox_events_failed ON outbox_events (id) WHERE failed_at IS NOT NULL;
