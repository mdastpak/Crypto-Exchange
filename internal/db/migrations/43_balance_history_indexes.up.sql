-- The history of a user's balances (#365) pages newest first, by the time
-- of a change and its ID, filtered by the user and, optionally, the
-- account, the currency, the kind of the entry, and a time range. Each
-- page is one range scan of an index that matches the order:
--
--   - balance_changes_user_history for the user, with any other filter
--     applied to the rows of the scan;
--   - balance_changes_user_currency_history when a currency is given.
--
-- They replace the index of the user alone, whose queries the first one
-- answers too.
CREATE INDEX balance_changes_user_history ON balance_changes (user_id, "timestamp" DESC, id DESC);
CREATE INDEX balance_changes_user_currency_history ON balance_changes (user_id, currency_hk, "timestamp" DESC, id DESC);
DROP INDEX IF EXISTS idx_balance_changes_user_id;
