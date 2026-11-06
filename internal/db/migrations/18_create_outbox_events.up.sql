-- Transactional outbox (#89). A change writes the messages it causes in the
-- same transaction, so a message exists if and only if its change commits.
-- The relay of internal/outbox publishes them afterwards, in id order per
-- topic, and retries while the broker or the cache is unavailable. One
-- server instance at a time relays, under an advisory lock.
CREATE TABLE outbox_events (
    id              BIGSERIAL   PRIMARY KEY,
    -- Says where the message goes and what runs before it is published.
    topic           TEXT        NOT NULL CHECK (topic <> ''),
    payload         JSONB       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Failed attempts, the time of the next one, and the last error.
    attempts        INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error      TEXT,
    -- Set once the message is published. Published rows are deleted after
    -- a retention period.
    published_at    TIMESTAMPTZ
);

-- The relay reads the pending messages in id order.
CREATE INDEX outbox_events_pending ON outbox_events (id) WHERE published_at IS NULL;
-- The cleanup finds the published messages by age.
CREATE INDEX outbox_events_published ON outbox_events (published_at) WHERE published_at IS NOT NULL;
