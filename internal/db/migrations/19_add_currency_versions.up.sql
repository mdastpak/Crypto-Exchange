-- Optimistic concurrency for currencies (#89). version counts the changes of
-- a currency: every update and the delete add one. A client that sends the
-- version its change is based on gets 409 when the currency changed since,
-- instead of overwriting a change it has not seen.
ALTER TABLE currencies ADD COLUMN version BIGINT NOT NULL DEFAULT 1 CHECK (version >= 1);
