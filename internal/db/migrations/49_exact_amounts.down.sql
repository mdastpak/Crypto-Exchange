-- Back to NUMERIC(36, 18) and the other fixed scales of the first
-- migrations. A value with more decimals is rounded, as those types do.
ALTER TABLE custody_configs DROP CONSTRAINT custody_configs_decimals_range;
ALTER TABLE assets DROP CONSTRAINT assets_decimals_range;

-- The checks on these columns are made again after their types change, so
-- that they compare the values as written rather than through a cast.
ALTER TABLE balance_changes DROP CONSTRAINT balance_changes_valid;
ALTER TABLE balance_snapshots DROP CONSTRAINT balance_snapshots_non_negative;
ALTER TABLE balances DROP CONSTRAINT balances_non_negative;
ALTER TABLE currency_delistings DROP CONSTRAINT currency_delistings_fixed_price_check;
ALTER TABLE currency_migration_sources DROP CONSTRAINT currency_migration_sources_ratio_check;

DROP INDEX IF EXISTS idx_balances_total;
ALTER TABLE balances DROP COLUMN total;

ALTER TABLE aml_rules
    ALTER COLUMN threshold_amount TYPE NUMERIC(36, 18);

ALTER TABLE assets
    ALTER COLUMN min_withdrawal TYPE NUMERIC(36, 18),
    ALTER COLUMN max_withdrawal TYPE NUMERIC(36, 18),
    ALTER COLUMN withdrawal_fee TYPE NUMERIC(36, 18);

ALTER TABLE balance_changes
    ALTER COLUMN amount TYPE NUMERIC(36, 18),
    ALTER COLUMN old_available TYPE NUMERIC(36, 18),
    ALTER COLUMN new_available TYPE NUMERIC(36, 18),
    ALTER COLUMN old_locked TYPE NUMERIC(36, 18),
    ALTER COLUMN new_locked TYPE NUMERIC(36, 18),
    ALTER COLUMN old_pending TYPE NUMERIC(36, 18),
    ALTER COLUMN new_pending TYPE NUMERIC(36, 18);

ALTER TABLE balance_snapshots
    ALTER COLUMN available TYPE NUMERIC(36, 18),
    ALTER COLUMN locked TYPE NUMERIC(36, 18),
    ALTER COLUMN pending_deposit TYPE NUMERIC(36, 18),
    ALTER COLUMN total TYPE NUMERIC(36, 18);

ALTER TABLE balances
    ALTER COLUMN available TYPE NUMERIC(36, 18),
    ALTER COLUMN locked TYPE NUMERIC(36, 18),
    ALTER COLUMN pending_deposit TYPE NUMERIC(36, 18);

ALTER TABLE currency_delistings
    ALTER COLUMN fixed_price TYPE NUMERIC(38, 18);

ALTER TABLE currency_migration_sources
    ALTER COLUMN ratio TYPE NUMERIC(38, 18);

ALTER TABLE custody_configs
    ALTER COLUMN min_deposit_amount TYPE NUMERIC(36, 18),
    ALTER COLUMN min_withdrawal_amount TYPE NUMERIC(36, 18),
    ALTER COLUMN max_withdrawal_amount TYPE NUMERIC(36, 18);

ALTER TABLE deposits
    ALTER COLUMN amount TYPE NUMERIC(36, 18),
    ALTER COLUMN confirmed_amount TYPE NUMERIC(36, 18);

ALTER TABLE market_stats_24h
    ALTER COLUMN last_price TYPE NUMERIC(36, 18),
    ALTER COLUMN open_price TYPE NUMERIC(36, 18),
    ALTER COLUMN high_price TYPE NUMERIC(36, 18),
    ALTER COLUMN low_price TYPE NUMERIC(36, 18),
    ALTER COLUMN price_change TYPE NUMERIC(36, 18),
    ALTER COLUMN price_change_pct TYPE NUMERIC(18, 8),
    ALTER COLUMN base_volume TYPE NUMERIC(36, 18),
    ALTER COLUMN quote_volume TYPE NUMERIC(36, 18),
    ALTER COLUMN bid_price TYPE NUMERIC(36, 18),
    ALTER COLUMN bid_size TYPE NUMERIC(36, 18),
    ALTER COLUMN ask_price TYPE NUMERIC(36, 18),
    ALTER COLUMN ask_size TYPE NUMERIC(36, 18),
    ALTER COLUMN spread TYPE NUMERIC(36, 18),
    ALTER COLUMN funding_rate TYPE NUMERIC(18, 8);

ALTER TABLE markets
    ALTER COLUMN min_order_size TYPE NUMERIC(36, 18),
    ALTER COLUMN max_order_size TYPE NUMERIC(36, 18),
    ALTER COLUMN min_notional TYPE NUMERIC(36, 18),
    ALTER COLUMN tick_size TYPE NUMERIC(36, 18),
    ALTER COLUMN lot_size TYPE NUMERIC(36, 18),
    ALTER COLUMN max_position_size TYPE NUMERIC(36, 18);

ALTER TABLE order_events
    ALTER COLUMN old_filled TYPE NUMERIC(36, 18),
    ALTER COLUMN new_filled TYPE NUMERIC(36, 18),
    ALTER COLUMN trade_price TYPE NUMERIC(36, 18),
    ALTER COLUMN trade_qty TYPE NUMERIC(36, 18),
    ALTER COLUMN trade_fee TYPE NUMERIC(36, 18);

ALTER TABLE orders
    ALTER COLUMN price TYPE NUMERIC(36, 18),
    ALTER COLUMN stop_price TYPE NUMERIC(36, 18),
    ALTER COLUMN quantity TYPE NUMERIC(36, 18),
    ALTER COLUMN filled_quantity TYPE NUMERIC(36, 18),
    ALTER COLUMN fee_paid TYPE NUMERIC(36, 18);

ALTER TABLE settlements
    ALTER COLUMN amount TYPE NUMERIC(36, 18);

ALTER TABLE sub_accounts
    ALTER COLUMN daily_volume_limit TYPE NUMERIC(36, 18),
    ALTER COLUMN daily_withdrawal_limit TYPE NUMERIC(36, 18),
    ALTER COLUMN position_limit TYPE NUMERIC(36, 18);

ALTER TABLE trade_events
    ALTER COLUMN price TYPE NUMERIC(36, 18),
    ALTER COLUMN quantity TYPE NUMERIC(36, 18),
    ALTER COLUMN maker_fee TYPE NUMERIC(36, 18),
    ALTER COLUMN taker_fee TYPE NUMERIC(36, 18);

ALTER TABLE trades
    ALTER COLUMN price TYPE NUMERIC(36, 18),
    ALTER COLUMN quantity TYPE NUMERIC(36, 18),
    ALTER COLUMN maker_fee TYPE NUMERIC(36, 18),
    ALTER COLUMN taker_fee TYPE NUMERIC(36, 18);

ALTER TABLE withdrawals
    ALTER COLUMN amount TYPE NUMERIC(36, 18),
    ALTER COLUMN custom_fee TYPE NUMERIC(36, 18),
    ALTER COLUMN estimated_fee TYPE NUMERIC(36, 18),
    ALTER COLUMN actual_fee TYPE NUMERIC(36, 18),
    ALTER COLUMN net_amount TYPE NUMERIC(36, 18);

ALTER TABLE balances ADD COLUMN total NUMERIC(36, 18) GENERATED ALWAYS AS (available + locked) STORED;
CREATE INDEX idx_balances_total ON balances(total DESC);

ALTER TABLE balances ADD CONSTRAINT balances_non_negative
    CHECK (available >= 0 AND locked >= 0 AND pending_deposit >= 0);
ALTER TABLE balance_snapshots ADD CONSTRAINT balance_snapshots_non_negative
    CHECK (available >= 0 AND locked >= 0 AND pending_deposit >= 0 AND total = available + locked);
ALTER TABLE balance_changes ADD CONSTRAINT balance_changes_valid CHECK (
    amount > 0
    AND old_available >= 0 AND new_available >= 0
    AND old_locked >= 0 AND new_locked >= 0
    AND old_pending >= 0 AND new_pending >= 0
);
ALTER TABLE currency_delistings ADD CONSTRAINT currency_delistings_fixed_price_check CHECK (fixed_price > 0);
ALTER TABLE currency_migration_sources ADD CONSTRAINT currency_migration_sources_ratio_check CHECK (ratio > 0);

DROP DOMAIN exact_decimal;
