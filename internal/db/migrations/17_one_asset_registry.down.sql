-- The references go back to a new, empty assets table, so the tables that
-- hold them must be empty, and the networks of the currencies are dropped.
DO $$
DECLARE
    t text;
    found boolean;
BEGIN
    FOREACH t IN ARRAY ARRAY['currency_networks', 'markets', 'balances', 'balance_changes', 'balance_snapshots', 'custody_configs',
                             'deposit_addresses', 'deposits', 'withdrawals', 'settlements', 'orders', 'trades'] LOOP
        EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I)', t) INTO found;
        IF found THEN
            RAISE EXCEPTION 'table % holds rows, which the down migration of 17_one_asset_registry would lose or could not move to assets', t;
        END IF;
    END LOOP;
END $$;

CREATE TYPE public.asset_status AS ENUM (
    'ACTIVE',
    'INACTIVE',
    'DEPRECATED',
    'DELISTED'
);

CREATE TYPE public.asset_type AS ENUM (
    'FIAT',
    'COIN',
    'TOKEN',
    'STABLE',
    'WRAPPED'
);

CREATE TABLE public.assets (
    id uuid DEFAULT public.uuid_generate_v4() NOT NULL,
    symbol character varying(20) NOT NULL,
    name character varying(100) NOT NULL,
    type public.asset_type NOT NULL,
    status public.asset_status DEFAULT 'INACTIVE'::public.asset_status NOT NULL,
    decimals smallint DEFAULT 8 NOT NULL,
    network character varying(50),
    contract_address character varying(100),
    native_symbol character varying(20),
    description text,
    logo_url character varying(500),
    website character varying(500),
    explorer_url character varying(500),
    min_withdrawal public.exact_decimal DEFAULT 0 NOT NULL,
    max_withdrawal public.exact_decimal DEFAULT 0 NOT NULL,
    withdrawal_fee public.exact_decimal DEFAULT 0 NOT NULL,
    deposit_enabled boolean DEFAULT false NOT NULL,
    withdrawal_enabled boolean DEFAULT false NOT NULL,
    custody_mode public.settlement_mode DEFAULT 'OFF_CHAIN'::public.settlement_mode NOT NULL,
    requires_tag boolean DEFAULT false NOT NULL,
    sanctions_screened boolean DEFAULT false NOT NULL,
    last_screened_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    deleted_at timestamp with time zone,
    CONSTRAINT assets_decimals_range CHECK (((decimals >= 0) AND (decimals <= 36)))
);

ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_pkey PRIMARY KEY (id);

ALTER TABLE ONLY public.assets
    ADD CONSTRAINT assets_symbol_key UNIQUE (symbol);

CREATE INDEX idx_assets_contract ON public.assets USING btree (contract_address);

CREATE INDEX idx_assets_deleted ON public.assets USING btree (deleted_at) WHERE (deleted_at IS NOT NULL);

CREATE INDEX idx_assets_network ON public.assets USING btree (network);

CREATE INDEX idx_assets_symbol ON public.assets USING btree (symbol);

CREATE INDEX idx_assets_type_status ON public.assets USING btree (type, status);

CREATE TRIGGER update_assets_updated_at BEFORE UPDATE ON public.assets FOR EACH ROW EXECUTE FUNCTION public.update_updated_at_column();

ALTER TABLE trades DROP CONSTRAINT trades_maker_fee_currency_hk_fkey, DROP CONSTRAINT trades_taker_fee_currency_hk_fkey;
ALTER TABLE trades RENAME COLUMN maker_fee_currency_hk TO maker_fee_asset_id;
ALTER TABLE trades RENAME COLUMN taker_fee_currency_hk TO taker_fee_asset_id;
ALTER TABLE trades
    ADD CONSTRAINT trades_maker_fee_asset_id_fkey FOREIGN KEY (maker_fee_asset_id) REFERENCES public.assets(id),
    ADD CONSTRAINT trades_taker_fee_asset_id_fkey FOREIGN KEY (taker_fee_asset_id) REFERENCES public.assets(id);

ALTER TABLE orders DROP CONSTRAINT orders_fee_currency_hk_fkey;
ALTER TABLE orders RENAME COLUMN fee_currency_hk TO fee_asset_id;
ALTER TABLE orders ADD CONSTRAINT orders_fee_asset_id_fkey FOREIGN KEY (fee_asset_id) REFERENCES public.assets(id);

ALTER TABLE settlements DROP CONSTRAINT settlements_currency_hk_fkey;
ALTER TABLE settlements RENAME COLUMN currency_hk TO asset_id;
ALTER INDEX idx_settlements_currency RENAME TO idx_settlements_asset;
ALTER TABLE settlements ADD CONSTRAINT settlements_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE withdrawals DROP CONSTRAINT withdrawals_currency_hk_fkey;
ALTER TABLE withdrawals RENAME COLUMN currency_hk TO asset_id;
ALTER INDEX idx_withdrawals_currency_hk RENAME TO idx_withdrawals_asset_id;
ALTER TABLE withdrawals ADD CONSTRAINT withdrawals_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE deposits DROP CONSTRAINT deposits_currency_hk_fkey;
ALTER TABLE deposits RENAME COLUMN currency_hk TO asset_id;
ALTER TABLE deposits RENAME CONSTRAINT uq_deposits_tx_currency TO uq_deposit_tx_asset;
ALTER INDEX idx_deposits_currency_hk RENAME TO idx_deposits_asset_id;
ALTER TABLE deposits ADD CONSTRAINT deposits_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE deposit_addresses DROP CONSTRAINT deposit_addresses_currency_hk_fkey;
ALTER TABLE deposit_addresses RENAME COLUMN currency_hk TO asset_id;
ALTER TABLE deposit_addresses RENAME CONSTRAINT uq_deposit_addresses_user_currency_address TO uq_user_asset_address;
ALTER INDEX idx_deposit_addresses_currency_hk RENAME TO idx_deposit_addresses_asset_id;
ALTER TABLE deposit_addresses ADD CONSTRAINT deposit_addresses_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE custody_configs DROP CONSTRAINT custody_configs_currency_network_fkey;
ALTER INDEX idx_custody_configs_currency RENAME TO idx_custody_configs_asset;
ALTER TABLE custody_configs RENAME CONSTRAINT uq_custody_currency_network TO uq_custody_asset_network;
ALTER TABLE custody_configs RENAME COLUMN currency_hk TO asset_id;
ALTER TABLE custody_configs
    ADD COLUMN contract_address character varying(100),
    ADD COLUMN decimals smallint DEFAULT 18 NOT NULL,
    ADD COLUMN min_deposit_amount public.exact_decimal DEFAULT 0 NOT NULL,
    ADD COLUMN min_withdrawal_amount public.exact_decimal DEFAULT 0 NOT NULL,
    ADD COLUMN max_withdrawal_amount public.exact_decimal DEFAULT 0 NOT NULL,
    ADD COLUMN fee_asset_id uuid,
    ADD CONSTRAINT custody_configs_decimals_range CHECK (((decimals >= 0) AND (decimals <= 36))),
    ADD CONSTRAINT custody_configs_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id),
    ADD CONSTRAINT custody_configs_fee_asset_id_fkey FOREIGN KEY (fee_asset_id) REFERENCES public.assets(id);

ALTER TABLE balance_snapshots DROP CONSTRAINT balance_snapshots_currency_hk_fkey;
ALTER TABLE balance_snapshots RENAME COLUMN currency_hk TO asset_id;
ALTER TABLE balance_snapshots ADD CONSTRAINT balance_snapshots_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE balance_changes DROP CONSTRAINT balance_changes_currency_hk_fkey;
ALTER TABLE balance_changes RENAME COLUMN currency_hk TO asset_id;
ALTER TABLE balance_changes ADD CONSTRAINT balance_changes_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE balances DROP CONSTRAINT balances_currency_hk_fkey;
ALTER TABLE balances RENAME COLUMN currency_hk TO asset_id;
ALTER TABLE balances RENAME CONSTRAINT uq_balances_user_currency_sub TO uq_user_asset_sub;
ALTER INDEX idx_balances_currency_hk RENAME TO idx_balances_asset_id;
ALTER TABLE balances ADD CONSTRAINT balances_asset_id_fkey FOREIGN KEY (asset_id) REFERENCES public.assets(id);

ALTER TABLE markets DROP CONSTRAINT markets_base_currency_hk_fkey, DROP CONSTRAINT markets_quote_currency_hk_fkey;
ALTER INDEX idx_markets_quote_currency RENAME TO idx_markets_quote_asset;
ALTER INDEX idx_markets_base_currency RENAME TO idx_markets_base_asset;
ALTER TABLE markets RENAME CONSTRAINT markets_different_currencies TO chk_different_assets;
ALTER TABLE markets RENAME COLUMN quote_currency_hk TO quote_asset_id;
ALTER TABLE markets RENAME COLUMN base_currency_hk TO base_asset_id;
ALTER TABLE markets
    ADD CONSTRAINT markets_base_asset_id_fkey FOREIGN KEY (base_asset_id) REFERENCES public.assets(id),
    ADD CONSTRAINT markets_quote_asset_id_fkey FOREIGN KEY (quote_asset_id) REFERENCES public.assets(id);

DROP TABLE currency_networks;
