-- The OTC trades (#62). A taken quote is executed in one transaction: the
-- user's funds are locked and settled against the exchange's inventory by
-- the ledger, the trade is recorded here, sealed, with the parts of its
-- price, and its events are written to the outbox.
--
-- A trade is one row per quote: the quote is unique, and so is the
-- idempotency key of the user, whose retry finds the trade again. The row
-- is never changed after its seal; the seal covers what it settled.
CREATE TABLE otc_trades (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL REFERENCES users (hk),
    sub_account_id uuid,
    quote_id uuid NOT NULL,
    idempotency_key text NOT NULL CONSTRAINT otc_trades_idempotency_key_check CHECK (length(idempotency_key) BETWEEN 1 AND 100),
    -- The SHA-256 of the request, which a retry of the key must match.
    request_digest bytea NOT NULL,
    market text NOT NULL,
    base_currency_hk uuid NOT NULL REFERENCES currencies (hk),
    quote_currency_hk uuid NOT NULL REFERENCES currencies (hk),
    side text NOT NULL CONSTRAINT otc_trades_side_check CHECK (side IN ('buy', 'sell')),
    -- As quoted: the quantity in the base currency; the price, the total
    -- (quantity × price), and the fee in the quote currency.
    quantity exact_decimal NOT NULL CONSTRAINT otc_trades_quantity_check CHECK (quantity::numeric > 0),
    price exact_decimal NOT NULL CONSTRAINT otc_trades_price_check CHECK (price::numeric > 0),
    total exact_decimal NOT NULL CONSTRAINT otc_trades_total_check CHECK (total::numeric > 0),
    fee exact_decimal NOT NULL CONSTRAINT otc_trades_fee_check CHECK (fee::numeric >= 0),
    fee_bps integer NOT NULL CONSTRAINT otc_trades_fee_bps_check CHECK (fee_bps >= 0),
    fee_currency_hk uuid NOT NULL REFERENCES currencies (hk),
    -- What the user gave and received: a buy gives total + fee of the quote
    -- currency and receives the quantity; a sell gives the quantity and
    -- receives total - fee.
    give_amount exact_decimal NOT NULL CONSTRAINT otc_trades_give_amount_check CHECK (give_amount::numeric > 0),
    receive_amount exact_decimal NOT NULL CONSTRAINT otc_trades_receive_amount_check CHECK (receive_amount::numeric > 0),
    -- The parts of the price (otc.Components), never shown to the user,
    -- with the price override that the reference came from, if any.
    components jsonb NOT NULL,
    override_id text,
    -- The ledger entry of the settlement.
    entry_id uuid NOT NULL REFERENCES ledger_entries (id),
    quoted_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    -- The time of the execution, in microseconds, from the database's
    -- clock.
    executed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT otc_trades_quote_key UNIQUE (quote_id),
    CONSTRAINT otc_trades_user_key UNIQUE (user_id, idempotency_key),
    -- A sub-account is of the user (migration 20).
    CONSTRAINT otc_trades_sub_account_owner_fkey FOREIGN KEY (sub_account_id, user_id) REFERENCES sub_accounts (id, user_id)
);

CREATE INDEX otc_trades_user ON otc_trades (user_id, executed_at DESC);

-- The net position of the exchange's inventory in each currency from the
-- OTC trades: what users gave it less what it gave them. A negative
-- position is a short. The execution changes it under the lock of its row
-- and refuses a trade beyond otc.inventory_limits; the inventory account
-- of the ledger holds the same sum.
CREATE TABLE otc_house_positions (
    currency_hk uuid PRIMARY KEY REFERENCES currencies (hk),
    position exact_decimal NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER
);

-- The inventory that earlier trades of the ledger left, so that the
-- positions start from the books. Its rows are not sealed yet: the
-- verifier adopts them in report mode.
INSERT INTO otc_house_positions (currency_hk, position)
SELECT currency_hk, SUM(amount::numeric) FROM ledger_postings WHERE account = 'inventory' GROUP BY currency_hk;

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods',
            'user_anti_phishing_codes', 'sms_routing', 'currency_networks', 'markets', 'withdrawals', 'integrity_findings',
            'withdrawal_payouts', 'deny_links', 'price_overrides',
            'currency_migrations', 'currency_delistings',
            'otc_trades', 'otc_house_positions'));

-- A trade whose fee is in the currency that the user receives nets it from
-- what the user is credited (#62): the user is credited the received
-- amount less the fee, the inventory gives the whole of it, and the fees
-- account takes the fee. It takes nothing from another hold (#407).
CREATE OR REPLACE FUNCTION ledger_entry_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    entry uuid;
    unbalanced record;
    entry_kind text;
    shape text;
BEGIN
    IF TG_TABLE_NAME = 'ledger_entries' THEN
        entry := NEW.id;
    ELSE
        entry := NEW.entry_id;
    END IF;
    SELECT d.currency_hk, SUM(d.delta) AS total INTO unbalanced FROM (
        SELECT p.currency_hk, p.amount::numeric AS delta FROM ledger_postings p WHERE p.entry_id = entry
        UNION ALL
        SELECT c.currency_hk,
            (c.new_available::numeric + c.new_locked::numeric + c.new_pending::numeric)
            - (c.old_available::numeric + c.old_locked::numeric + c.old_pending::numeric)
        FROM balance_changes c WHERE c.entry_id = entry
    ) d GROUP BY d.currency_hk HAVING SUM(d.delta) <> 0 LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'ledger entry % does not balance in currency %: the postings sum to %', entry, unbalanced.currency_hk, unbalanced.total
            USING ERRCODE = 'check_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM balance_changes c WHERE c.entry_id = entry)
       AND NOT EXISTS (SELECT 1 FROM ledger_postings p WHERE p.entry_id = entry) THEN
        RAISE EXCEPTION 'ledger entry % moves nothing', entry USING ERRCODE = 'check_violation';
    END IF;
    -- The shape: the change types of the entry, then its postings as
    -- account and sign, each sorted, in the C collation so that the order
    -- depends on no setting of the database.
    SELECT e.kind INTO entry_kind FROM ledger_entries e WHERE e.id = entry;
    SELECT COALESCE((SELECT string_agg(c.change_type, ',' ORDER BY c.change_type COLLATE "C")
            FROM balance_changes c WHERE c.entry_id = entry), '')
        || '|' ||
        COALESCE((SELECT string_agg(s.posting, ',' ORDER BY s.posting COLLATE "C") FROM (
            SELECT p.account || CASE WHEN p.amount::numeric > 0 THEN '+' ELSE '-' END AS posting
            FROM ledger_postings p WHERE p.entry_id = entry) s), '')
        INTO shape;
    IF NOT shape = ANY (CASE entry_kind
        WHEN 'deposit_detected' THEN ARRAY['PENDING_DEPOSIT|deposits_pending-']
        WHEN 'deposit_credited' THEN ARRAY['DEPOSIT|custody-,deposits_pending+']
        WHEN 'deposit_failed' THEN ARRAY['DEPOSIT_FAILED|deposits_pending+']
        WHEN 'lock' THEN ARRAY['LOCK|']
        WHEN 'release' THEN ARRAY['UNLOCK|']
        WHEN 'transfer' THEN ARRAY['LOCK,TRANSFER_IN,TRANSFER_OUT|']
        WHEN 'trade' THEN ARRAY['TRADE_CREDIT,TRADE_DEBIT|inventory+,inventory-',
            'FEE,TRADE_CREDIT,TRADE_DEBIT|fees+,inventory+,inventory-',
            'TRADE_CREDIT,TRADE_DEBIT|fees+,inventory+,inventory-']
        WHEN 'withdrawal' THEN ARRAY['WITHDRAWAL|custody+', 'FEE,WITHDRAWAL|custody+,fees+']
        WHEN 'conversion' THEN ARRAY['CONVERSION_CREDIT,CONVERSION_DEBIT,LOCK|custody+,custody-',
            'CONVERSION_CREDIT,CONVERSION_DEBIT,LOCK|custody+,custody-,fees+',
            'CONVERSION_DEBIT,LOCK|custody+,custody-,fees+']
        ELSE ARRAY[]::text[] END) THEN
        RAISE EXCEPTION 'ledger entry % of kind % has the shape %, which no operation of the ledger writes', entry, entry_kind, shape
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;
