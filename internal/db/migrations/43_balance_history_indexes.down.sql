CREATE INDEX IF NOT EXISTS idx_balance_changes_user_id ON balance_changes (user_id);
DROP INDEX IF EXISTS balance_changes_user_currency_history;
DROP INDEX IF EXISTS balance_changes_user_history;
