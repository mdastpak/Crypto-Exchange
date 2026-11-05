-- Restores the balance schema of migration 7. The on-chain columns come back
-- as zero, and total becomes a plain column again, filled from its
-- computed value.
ALTER TABLE balance_changes DROP CONSTRAINT IF EXISTS balance_changes_valid;
ALTER TABLE balance_changes
    ADD COLUMN old_on_chain NUMERIC(36, 18) NOT NULL DEFAULT 0,
    ADD COLUMN new_on_chain NUMERIC(36, 18) NOT NULL DEFAULT 0;
ALTER TABLE balance_changes ALTER COLUMN old_on_chain DROP DEFAULT, ALTER COLUMN new_on_chain DROP DEFAULT;

ALTER TABLE balance_snapshots DROP CONSTRAINT IF EXISTS balance_snapshots_non_negative;
ALTER TABLE balance_snapshots ADD COLUMN on_chain NUMERIC(36, 18) NOT NULL DEFAULT 0;
ALTER TABLE balance_snapshots ALTER COLUMN on_chain DROP DEFAULT;

ALTER TABLE balances DROP CONSTRAINT IF EXISTS uq_user_asset_sub;
ALTER TABLE balances ADD CONSTRAINT uq_user_asset_sub UNIQUE (user_id, sub_account_id, asset_id);
ALTER TABLE balances DROP CONSTRAINT IF EXISTS balances_non_negative;
DROP INDEX IF EXISTS idx_balances_total;
ALTER TABLE balances RENAME COLUMN total TO total_generated;
ALTER TABLE balances ADD COLUMN total NUMERIC(36, 18) NOT NULL DEFAULT 0;
UPDATE balances SET total = total_generated;
ALTER TABLE balances DROP COLUMN total_generated;
CREATE INDEX idx_balances_total ON balances(total DESC);
ALTER TABLE balances ADD COLUMN on_chain NUMERIC(36, 18) NOT NULL DEFAULT 0;
