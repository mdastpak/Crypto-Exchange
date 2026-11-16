DROP INDEX IF EXISTS idx_balances_currency_user;
DROP INDEX IF EXISTS currency_delisting_notices_broadcasting;

ALTER TABLE currency_delisting_notices
    DROP COLUMN broadcast_stale,
    DROP COLUMN broadcast_no_contact,
    DROP COLUMN broadcast_sent,
    DROP COLUMN broadcast_done_at,
    DROP COLUMN broadcast_cursor,
    DROP COLUMN broadcast,
    DROP COLUMN prepare_error,
    DROP COLUMN prepare_retry_at,
    DROP COLUMN prepare_attempts,
    DROP COLUMN prepare_cursor;
