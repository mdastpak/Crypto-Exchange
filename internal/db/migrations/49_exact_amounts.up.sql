-- One representation for every amount, price, rate, and ratio of the
-- exchange's tables (#314, docs/architecture.md#amounts-prices-and-ratios).
-- NUMERIC(36, 18) rounded any value to 18 decimals without an error, so a
-- deposit of an asset with more decimals, such as NEAR (24), would have
-- been credited short. exact_decimal stores a value exactly as written, and
-- refuses, instead of rounding, a value with more than 36 decimals or 42
-- integer digits; an asset may have at most 36 decimals.
CREATE DOMAIN exact_decimal AS NUMERIC
    CHECK (scale(VALUE) <= 36 AND abs(VALUE) < 1e42);

-- The checks on these columns are made again after their types change, so
-- that they compare the values as written rather than through a cast.
ALTER TABLE balance_changes DROP CONSTRAINT balance_changes_valid;
ALTER TABLE balance_snapshots DROP CONSTRAINT balance_snapshots_non_negative;
ALTER TABLE balances DROP CONSTRAINT balances_non_negative;
ALTER TABLE currency_delistings DROP CONSTRAINT currency_delistings_fixed_price_check;
ALTER TABLE currency_migration_sources DROP CONSTRAINT currency_migration_sources_ratio_check;

-- total is computed from available and locked, so it goes while they change.
DROP INDEX IF EXISTS idx_balances_total;
ALTER TABLE balances DROP COLUMN total;

ALTER TABLE aml_rules
    ALTER COLUMN threshold_amount TYPE exact_decimal;

ALTER TABLE assets
    ALTER COLUMN min_withdrawal TYPE exact_decimal,
    ALTER COLUMN max_withdrawal TYPE exact_decimal,
    ALTER COLUMN withdrawal_fee TYPE exact_decimal;

ALTER TABLE balance_changes
    ALTER COLUMN amount TYPE exact_decimal,
    ALTER COLUMN old_available TYPE exact_decimal,
    ALTER COLUMN new_available TYPE exact_decimal,
    ALTER COLUMN old_locked TYPE exact_decimal,
    ALTER COLUMN new_locked TYPE exact_decimal,
    ALTER COLUMN old_pending TYPE exact_decimal,
    ALTER COLUMN new_pending TYPE exact_decimal;

ALTER TABLE balance_snapshots
    ALTER COLUMN available TYPE exact_decimal,
    ALTER COLUMN locked TYPE exact_decimal,
    ALTER COLUMN pending_deposit TYPE exact_decimal,
    ALTER COLUMN total TYPE exact_decimal;

ALTER TABLE balances
    ALTER COLUMN available TYPE exact_decimal,
    ALTER COLUMN locked TYPE exact_decimal,
    ALTER COLUMN pending_deposit TYPE exact_decimal;

ALTER TABLE currency_delistings
    ALTER COLUMN fixed_price TYPE exact_decimal;

ALTER TABLE currency_migration_sources
    ALTER COLUMN ratio TYPE exact_decimal;

ALTER TABLE custody_configs
    ALTER COLUMN min_deposit_amount TYPE exact_decimal,
    ALTER COLUMN min_withdrawal_amount TYPE exact_decimal,
    ALTER COLUMN max_withdrawal_amount TYPE exact_decimal;

ALTER TABLE deposits
    ALTER COLUMN amount TYPE exact_decimal,
    ALTER COLUMN confirmed_amount TYPE exact_decimal;

ALTER TABLE market_stats_24h
    ALTER COLUMN last_price TYPE exact_decimal,
    ALTER COLUMN open_price TYPE exact_decimal,
    ALTER COLUMN high_price TYPE exact_decimal,
    ALTER COLUMN low_price TYPE exact_decimal,
    ALTER COLUMN price_change TYPE exact_decimal,
    ALTER COLUMN price_change_pct TYPE exact_decimal,
    ALTER COLUMN base_volume TYPE exact_decimal,
    ALTER COLUMN quote_volume TYPE exact_decimal,
    ALTER COLUMN bid_price TYPE exact_decimal,
    ALTER COLUMN bid_size TYPE exact_decimal,
    ALTER COLUMN ask_price TYPE exact_decimal,
    ALTER COLUMN ask_size TYPE exact_decimal,
    ALTER COLUMN spread TYPE exact_decimal,
    ALTER COLUMN funding_rate TYPE exact_decimal;

ALTER TABLE markets
    ALTER COLUMN min_order_size TYPE exact_decimal,
    ALTER COLUMN max_order_size TYPE exact_decimal,
    ALTER COLUMN min_notional TYPE exact_decimal,
    ALTER COLUMN tick_size TYPE exact_decimal,
    ALTER COLUMN lot_size TYPE exact_decimal,
    ALTER COLUMN max_position_size TYPE exact_decimal;

ALTER TABLE order_events
    ALTER COLUMN old_filled TYPE exact_decimal,
    ALTER COLUMN new_filled TYPE exact_decimal,
    ALTER COLUMN trade_price TYPE exact_decimal,
    ALTER COLUMN trade_qty TYPE exact_decimal,
    ALTER COLUMN trade_fee TYPE exact_decimal;

ALTER TABLE orders
    ALTER COLUMN price TYPE exact_decimal,
    ALTER COLUMN stop_price TYPE exact_decimal,
    ALTER COLUMN quantity TYPE exact_decimal,
    ALTER COLUMN filled_quantity TYPE exact_decimal,
    ALTER COLUMN fee_paid TYPE exact_decimal;

ALTER TABLE settlements
    ALTER COLUMN amount TYPE exact_decimal;

ALTER TABLE sub_accounts
    ALTER COLUMN daily_volume_limit TYPE exact_decimal,
    ALTER COLUMN daily_withdrawal_limit TYPE exact_decimal,
    ALTER COLUMN position_limit TYPE exact_decimal;

ALTER TABLE trade_events
    ALTER COLUMN price TYPE exact_decimal,
    ALTER COLUMN quantity TYPE exact_decimal,
    ALTER COLUMN maker_fee TYPE exact_decimal,
    ALTER COLUMN taker_fee TYPE exact_decimal;

ALTER TABLE trades
    ALTER COLUMN price TYPE exact_decimal,
    ALTER COLUMN quantity TYPE exact_decimal,
    ALTER COLUMN maker_fee TYPE exact_decimal,
    ALTER COLUMN taker_fee TYPE exact_decimal;

ALTER TABLE withdrawals
    ALTER COLUMN amount TYPE exact_decimal,
    ALTER COLUMN custom_fee TYPE exact_decimal,
    ALTER COLUMN estimated_fee TYPE exact_decimal,
    ALTER COLUMN actual_fee TYPE exact_decimal,
    ALTER COLUMN net_amount TYPE exact_decimal;

ALTER TABLE balances ADD COLUMN total exact_decimal GENERATED ALWAYS AS (available + locked) STORED;
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

ALTER TABLE assets ADD CONSTRAINT assets_decimals_range CHECK (decimals BETWEEN 0 AND 36);
ALTER TABLE custody_configs ADD CONSTRAINT custody_configs_decimals_range CHECK (decimals BETWEEN 0 AND 36);
