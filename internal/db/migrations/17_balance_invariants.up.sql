-- Migration 17 makes the database enforce the balance model of #43.
--
-- A balance has available, locked, and pending_deposit funds; none may be
-- negative, and total, what the user owns, is available + locked, computed
-- by the database. on_chain, the custody location of funds, is not part of
-- a user's balance: custody tracks it per wallet. A user has one balance per
-- asset and sub-account, also without a sub-account.

-- Preflight: stop before changing anything if rows hold data that this
-- migration would drop or that breaks the new rules. Nothing writes these
-- tables yet.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM balances WHERE on_chain <> 0)
       OR EXISTS (SELECT 1 FROM balance_snapshots WHERE on_chain <> 0)
       OR EXISTS (SELECT 1 FROM balance_changes WHERE old_on_chain <> 0 OR new_on_chain <> 0) THEN
        RAISE EXCEPTION 'migration 17: balance rows hold on-chain amounts, which this migration drops; move them into the custody records, then run "migrate force 16" and start the server again';
    END IF;
    IF EXISTS (SELECT 1 FROM balances WHERE available < 0 OR locked < 0 OR pending_deposit < 0) THEN
        RAISE EXCEPTION 'migration 17: balance rows have negative amounts; correct them, then run "migrate force 16" and start the server again';
    END IF;
    IF EXISTS (SELECT 1 FROM balances WHERE sub_account_id IS NULL GROUP BY user_id, asset_id HAVING COUNT(*) > 1) THEN
        RAISE EXCEPTION 'migration 17: some users have several balances of one asset without a sub-account; merge them, then run "migrate force 16" and start the server again';
    END IF;
END
$$;

ALTER TABLE balances DROP COLUMN on_chain;
DROP INDEX IF EXISTS idx_balances_total;
ALTER TABLE balances DROP COLUMN total;
ALTER TABLE balances ADD COLUMN total NUMERIC(36, 18) GENERATED ALWAYS AS (available + locked) STORED;
CREATE INDEX idx_balances_total ON balances(total DESC);
ALTER TABLE balances ADD CONSTRAINT balances_non_negative
    CHECK (available >= 0 AND locked >= 0 AND pending_deposit >= 0);
-- NULL sub-accounts are equal here; otherwise every NULL counts as distinct
-- and a user could get several balances of one asset.
ALTER TABLE balances DROP CONSTRAINT uq_user_asset_sub;
ALTER TABLE balances ADD CONSTRAINT uq_user_asset_sub UNIQUE NULLS NOT DISTINCT (user_id, sub_account_id, asset_id);

ALTER TABLE balance_snapshots DROP COLUMN on_chain;
ALTER TABLE balance_snapshots ADD CONSTRAINT balance_snapshots_non_negative
    CHECK (available >= 0 AND locked >= 0 AND pending_deposit >= 0 AND total = available + locked);

ALTER TABLE balance_changes DROP COLUMN old_on_chain, DROP COLUMN new_on_chain;
ALTER TABLE balance_changes ADD CONSTRAINT balance_changes_valid CHECK (
    amount > 0
    AND old_available >= 0 AND new_available >= 0
    AND old_locked >= 0 AND new_locked >= 0
    AND old_pending >= 0 AND new_pending >= 0
);
