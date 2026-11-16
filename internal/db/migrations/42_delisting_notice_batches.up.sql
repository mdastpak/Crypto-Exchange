-- The notices of delistings in batches (#461). A notice was fanned out to
-- every user in one statement, which times out with enough users, and
-- wrote one row per user, kept forever. Now:
--
-- - the holders of the currency are written as recipients in keyset
--   batches over balances (prepare_cursor), one transaction each, and the
--   notice is prepared when the last batch is written;
-- - the other users of a notice that goes to everyone are sent in keyset
--   batches over users (broadcast_cursor), counted on the notice, and
--   written as a recipient only when their send fails, to be tried again;
-- - a notice that fails to prepare waits for prepare_retry_at, and the
--   others go on.
ALTER TABLE currency_delisting_notices
    ADD COLUMN prepare_cursor uuid,
    ADD COLUMN prepare_attempts integer NOT NULL DEFAULT 0 CHECK (prepare_attempts >= 0),
    ADD COLUMN prepare_retry_at TIMESTAMPTZ,
    ADD COLUMN prepare_error text CHECK (char_length(prepare_error) <= 200),
    ADD COLUMN broadcast boolean NOT NULL DEFAULT false,
    ADD COLUMN broadcast_cursor uuid,
    ADD COLUMN broadcast_done_at TIMESTAMPTZ,
    ADD COLUMN broadcast_sent integer NOT NULL DEFAULT 0 CHECK (broadcast_sent >= 0),
    ADD COLUMN broadcast_no_contact integer NOT NULL DEFAULT 0 CHECK (broadcast_no_contact >= 0),
    ADD COLUMN broadcast_stale integer NOT NULL DEFAULT 0 CHECK (broadcast_stale >= 0);

-- A notice that was prepared before this migration wrote every user as a
-- recipient: it has nothing to broadcast.

CREATE INDEX currency_delisting_notices_broadcasting ON currency_delisting_notices (due_at)
    WHERE broadcast AND broadcast_done_at IS NULL;

-- The holders of a currency, in the order of their user.
CREATE INDEX idx_balances_currency_user ON balances (currency_hk, user_id);
