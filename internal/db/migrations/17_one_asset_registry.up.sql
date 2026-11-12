-- One registry of the assets the exchange supports (#309): currencies.
--
-- The schema had two: currencies, which the admin API, the public list,
-- the restrictions, the favorites of currencies, and the delistings use,
-- and assets, which the markets, the balances, the orders, the trades, and
-- custody referenced. So one coin had two rows, and the ledger would have
-- moved money keyed by one while a delisting or a hold was keyed by the
-- other. currencies has the lifecycle, the versions, the audit log, and the
-- cache, so it stays; assets goes.
--
-- How a currency moves on each network, with its contract, its decimals,
-- and its limits, goes to currency_networks. The custody configuration
-- keeps how to reach a network, and refers to the network of the currency
-- for the rest, so that no second table holds a contract or decimals.
--
-- Nothing writes the tables that referenced assets, so they are empty; this
-- migration refuses to run otherwise, rather than guess which currency a
-- row meant.
DO $$
DECLARE
    t text;
    found boolean;
BEGIN
    FOREACH t IN ARRAY ARRAY['assets', 'markets', 'balances', 'balance_changes', 'balance_snapshots', 'custody_configs',
                             'deposit_addresses', 'deposits', 'withdrawals', 'settlements', 'orders', 'trades'] LOOP
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I)', t) INTO found;
        IF found THEN
            RAISE EXCEPTION 'table % holds rows; this migration moves its references from assets to currencies and cannot tell which currency they mean (#309)', t;
        END IF;
    END LOOP;
END $$;

CREATE TABLE currency_networks (
    currency_hk uuid NOT NULL REFERENCES currencies (hk),
    network character varying(50) NOT NULL CONSTRAINT currency_networks_network_check CHECK (network ~ '^[a-z][a-z0-9_]*$'),
    -- NULL for the native asset of the network, and on networks without
    -- contracts.
    contract_address character varying(100),
    decimals smallint NOT NULL CONSTRAINT currency_networks_decimals_check CHECK (decimals >= 0 AND decimals <= 36),
    custody_mode settlement_mode NOT NULL,
    deposit_enabled boolean NOT NULL DEFAULT false,
    withdrawal_enabled boolean NOT NULL DEFAULT false,
    min_deposit exact_decimal NOT NULL DEFAULT 0,
    min_withdrawal exact_decimal NOT NULL DEFAULT 0,
    -- 0 means no maximum.
    max_withdrawal exact_decimal NOT NULL DEFAULT 0,
    withdrawal_fee exact_decimal NOT NULL DEFAULT 0,
    -- The currency that pays the network fee, such as TRX for a TRC20 token.
    fee_currency_hk uuid REFERENCES currencies (hk),
    requires_tag boolean NOT NULL DEFAULT false,
    explorer_url character varying(500),
    created_at timestamp with time zone NOT NULL DEFAULT now(),
    updated_at timestamp with time zone NOT NULL DEFAULT now(),
    CONSTRAINT currency_networks_pkey PRIMARY KEY (currency_hk, network),
    CONSTRAINT currency_networks_amounts_check CHECK (
        min_deposit::numeric >= 0 AND min_withdrawal::numeric >= 0 AND max_withdrawal::numeric >= 0
        AND withdrawal_fee::numeric >= 0
        AND (max_withdrawal::numeric = 0 OR max_withdrawal::numeric >= min_withdrawal::numeric))
);

-- A contract is one currency's, on its network.
CREATE UNIQUE INDEX currency_networks_contract ON currency_networks (network, lower(contract_address))
    WHERE contract_address IS NOT NULL;

CREATE TRIGGER update_currency_networks_updated_at BEFORE UPDATE ON currency_networks
    FOR EACH ROW EXECUTE FUNCTION update_updated_at_column();

-- The markets.
ALTER TABLE markets DROP CONSTRAINT markets_base_asset_id_fkey, DROP CONSTRAINT markets_quote_asset_id_fkey;
ALTER TABLE markets RENAME COLUMN base_asset_id TO base_currency_hk;
ALTER TABLE markets RENAME COLUMN quote_asset_id TO quote_currency_hk;
ALTER TABLE markets RENAME CONSTRAINT chk_different_assets TO markets_different_currencies;
ALTER INDEX idx_markets_base_asset RENAME TO idx_markets_base_currency;
ALTER INDEX idx_markets_quote_asset RENAME TO idx_markets_quote_currency;
ALTER TABLE markets
    ADD CONSTRAINT markets_base_currency_hk_fkey FOREIGN KEY (base_currency_hk) REFERENCES currencies (hk),
    ADD CONSTRAINT markets_quote_currency_hk_fkey FOREIGN KEY (quote_currency_hk) REFERENCES currencies (hk);

-- The balances and their history.
ALTER TABLE balances DROP CONSTRAINT balances_asset_id_fkey;
ALTER TABLE balances RENAME COLUMN asset_id TO currency_hk;
ALTER TABLE balances RENAME CONSTRAINT uq_user_asset_sub TO uq_balances_user_currency_sub;
ALTER INDEX idx_balances_asset_id RENAME TO idx_balances_currency_hk;
ALTER TABLE balances ADD CONSTRAINT balances_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES currencies (hk);

ALTER TABLE balance_changes DROP CONSTRAINT balance_changes_asset_id_fkey;
ALTER TABLE balance_changes RENAME COLUMN asset_id TO currency_hk;
ALTER TABLE balance_changes ADD CONSTRAINT balance_changes_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES currencies (hk);

ALTER TABLE balance_snapshots DROP CONSTRAINT balance_snapshots_asset_id_fkey;
ALTER TABLE balance_snapshots RENAME COLUMN asset_id TO currency_hk;
ALTER TABLE balance_snapshots ADD CONSTRAINT balance_snapshots_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES currencies (hk);

-- Custody: the configuration of a network refers to the network of the
-- currency, which holds the contract, the decimals, the limits, and the
-- currency of the fee.
ALTER TABLE custody_configs
    DROP CONSTRAINT custody_configs_asset_id_fkey,
    DROP CONSTRAINT custody_configs_fee_asset_id_fkey,
    DROP CONSTRAINT custody_configs_decimals_range,
    DROP COLUMN fee_asset_id,
    DROP COLUMN contract_address,
    DROP COLUMN decimals,
    DROP COLUMN min_deposit_amount,
    DROP COLUMN min_withdrawal_amount,
    DROP COLUMN max_withdrawal_amount;
ALTER TABLE custody_configs RENAME COLUMN asset_id TO currency_hk;
ALTER TABLE custody_configs RENAME CONSTRAINT uq_custody_asset_network TO uq_custody_currency_network;
ALTER INDEX idx_custody_configs_asset RENAME TO idx_custody_configs_currency;
ALTER TABLE custody_configs ADD CONSTRAINT custody_configs_currency_network_fkey
    FOREIGN KEY (currency_hk, network) REFERENCES currency_networks (currency_hk, network);

ALTER TABLE deposit_addresses DROP CONSTRAINT deposit_addresses_asset_id_fkey;
ALTER TABLE deposit_addresses RENAME COLUMN asset_id TO currency_hk;
ALTER TABLE deposit_addresses RENAME CONSTRAINT uq_user_asset_address TO uq_deposit_addresses_user_currency_address;
ALTER INDEX idx_deposit_addresses_asset_id RENAME TO idx_deposit_addresses_currency_hk;
ALTER TABLE deposit_addresses ADD CONSTRAINT deposit_addresses_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES currencies (hk);

ALTER TABLE deposits DROP CONSTRAINT deposits_asset_id_fkey;
ALTER TABLE deposits RENAME COLUMN asset_id TO currency_hk;
ALTER TABLE deposits RENAME CONSTRAINT uq_deposit_tx_asset TO uq_deposits_tx_currency;
ALTER INDEX idx_deposits_asset_id RENAME TO idx_deposits_currency_hk;
ALTER TABLE deposits ADD CONSTRAINT deposits_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES currencies (hk);

ALTER TABLE withdrawals DROP CONSTRAINT withdrawals_asset_id_fkey;
ALTER TABLE withdrawals RENAME COLUMN asset_id TO currency_hk;
ALTER INDEX idx_withdrawals_asset_id RENAME TO idx_withdrawals_currency_hk;
ALTER TABLE withdrawals ADD CONSTRAINT withdrawals_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES currencies (hk);

ALTER TABLE settlements DROP CONSTRAINT settlements_asset_id_fkey;
ALTER TABLE settlements RENAME COLUMN asset_id TO currency_hk;
ALTER INDEX idx_settlements_asset RENAME TO idx_settlements_currency;
ALTER TABLE settlements ADD CONSTRAINT settlements_currency_hk_fkey FOREIGN KEY (currency_hk) REFERENCES currencies (hk);

-- The currencies that fees are paid in.
ALTER TABLE orders DROP CONSTRAINT orders_fee_asset_id_fkey;
ALTER TABLE orders RENAME COLUMN fee_asset_id TO fee_currency_hk;
ALTER TABLE orders ADD CONSTRAINT orders_fee_currency_hk_fkey FOREIGN KEY (fee_currency_hk) REFERENCES currencies (hk);

ALTER TABLE trades DROP CONSTRAINT trades_maker_fee_asset_id_fkey, DROP CONSTRAINT trades_taker_fee_asset_id_fkey;
ALTER TABLE trades RENAME COLUMN maker_fee_asset_id TO maker_fee_currency_hk;
ALTER TABLE trades RENAME COLUMN taker_fee_asset_id TO taker_fee_currency_hk;
ALTER TABLE trades
    ADD CONSTRAINT trades_maker_fee_currency_hk_fkey FOREIGN KEY (maker_fee_currency_hk) REFERENCES currencies (hk),
    ADD CONSTRAINT trades_taker_fee_currency_hk_fkey FOREIGN KEY (taker_fee_currency_hk) REFERENCES currencies (hk);

-- The second registry goes.
DROP TABLE assets;
DROP TYPE asset_status;
DROP TYPE asset_type;
