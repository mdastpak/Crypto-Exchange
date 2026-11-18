-- The routed orders of the OTC trades (#387): the order that covers the
-- venue leg of a trade's route (#386) at the venue, written with the trade,
-- and driven to the end by one node at a time.
CREATE TABLE otc_routed_orders (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    trade_id uuid NOT NULL REFERENCES otc_trades (id),
    -- The leg of the trade's route, and the attempt: an order whose
    -- remainder is routed again gets a child of the next attempt.
    leg integer NOT NULL CONSTRAINT otc_routed_orders_leg_check CHECK (leg >= 0),
    attempt integer NOT NULL DEFAULT 0 CONSTRAINT otc_routed_orders_attempt_check CHECK (attempt >= 0),
    parent_id uuid REFERENCES otc_routed_orders (id),
    venue text NOT NULL CONSTRAINT otc_routed_orders_venue_check CHECK (venue ~ '^[a-z][a-z0-9_]{0,29}$'),
    -- The venue's market, and its assets of our currencies.
    symbol text NOT NULL CONSTRAINT otc_routed_orders_symbol_check CHECK (length(symbol) BETWEEN 1 AND 40),
    base_asset text NOT NULL,
    quote_asset text NOT NULL,
    base_currency_hk uuid NOT NULL REFERENCES currencies (hk),
    quote_currency_hk uuid NOT NULL REFERENCES currencies (hk),
    -- The side at the venue: the side of the user, whom the inventory
    -- gave what the venue now buys or sells.
    side text NOT NULL CONSTRAINT otc_routed_orders_side_check CHECK (side IN ('buy', 'sell')),
    quantity exact_decimal NOT NULL CONSTRAINT otc_routed_orders_quantity_check CHECK (quantity::numeric > 0),
    -- The slippage guard: the limit of the order, the worst price at which
    -- it may fill.
    limit_price exact_decimal NOT NULL CONSTRAINT otc_routed_orders_limit_price_check CHECK (limit_price::numeric > 0),
    -- The trade's price to the user and the reference price of its quote,
    -- which bound the limit of an order that routes a remainder again.
    trade_price exact_decimal NOT NULL CONSTRAINT otc_routed_orders_trade_price_check CHECK (trade_price::numeric > 0),
    reference_price exact_decimal NOT NULL CONSTRAINT otc_routed_orders_reference_price_check CHECK (reference_price::numeric > 0),
    -- Our id of the order at the venue, derived from the trade, the leg,
    -- and the attempt, by which the order is looked up after a crash.
    client_id text NOT NULL CONSTRAINT otc_routed_orders_client_id_check CHECK (client_id ~ '^[A-Za-z0-9_-]{1,36}$'),
    venue_order_id text,
    status text NOT NULL DEFAULT 'planned' CONSTRAINT otc_routed_orders_status_check CHECK (status IN (
        'planned', 'placed', 'partially_filled', 'filled', 'withdrawal_requested', 'withdrawn', 'confirmed',
        'failed', 'cancelled', 'manual_review')),
    -- What the venue filled, in the base asset and in the quote asset, as
    -- the ledger posted it (ledger venue_fill).
    filled exact_decimal NOT NULL DEFAULT 0 CONSTRAINT otc_routed_orders_filled_check CHECK (filled::numeric >= 0),
    filled_quote exact_decimal NOT NULL DEFAULT 0 CONSTRAINT otc_routed_orders_filled_quote_check CHECK (filled_quote::numeric >= 0),
    -- The reservation of the venue balance that the order spends (#385).
    reservation_id uuid REFERENCES venue_reservations (id),
    -- The reason of a failure or a review: a code, never the venue's text.
    reason text CONSTRAINT otc_routed_orders_reason_check CHECK (length(reason) BETWEEN 1 AND 64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- Not sealed: the lease of the node that drives the order, and when it
    -- runs next. A node writes the order only with the token of its lease.
    lease_holder text,
    lease_token bigint NOT NULL DEFAULT 0,
    lease_until TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- When a call to place the order ended with an unknown outcome; the
    -- order is placed again only after the grace of the venue's lookup.
    uncertain_since TIMESTAMPTZ,
    placed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT otc_routed_orders_client_key UNIQUE (venue, client_id),
    CONSTRAINT otc_routed_orders_leg_key UNIQUE (trade_id, leg, attempt),
    CONSTRAINT otc_routed_orders_filled_within CHECK (filled::numeric <= quantity::numeric)
);

-- The orders that a node still drives.
CREATE INDEX otc_routed_orders_open ON otc_routed_orders (venue, next_attempt_at)
    WHERE status IN ('planned', 'placed', 'partially_filled');

-- Each transition of an order, with its time and the node that made it,
-- append-only.
CREATE TABLE otc_routed_order_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id uuid NOT NULL REFERENCES otc_routed_orders (id),
    from_status text,
    to_status text NOT NULL,
    node text NOT NULL CONSTRAINT otc_routed_order_events_node_check CHECK (length(node) BETWEEN 1 AND 200),
    lease_token bigint NOT NULL,
    reason text CONSTRAINT otc_routed_order_events_reason_check CHECK (length(reason) BETWEEN 1 AND 64),
    at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX otc_routed_order_events_order ON otc_routed_order_events (order_id, id);

CREATE FUNCTION otc_routed_order_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'otc_routed_order_events is append-only' USING ERRCODE = 'insufficient_privilege';
END $$;

CREATE TRIGGER otc_routed_order_events_append_only BEFORE UPDATE OR DELETE ON otc_routed_order_events
    FOR EACH ROW EXECUTE FUNCTION otc_routed_order_events_append_only();

-- The fills of the routed orders in the ledger: the venue gets what it
-- bought and gives what it spent, and the inventory the reverse.
ALTER TABLE ledger_entries
    DROP CONSTRAINT ledger_entries_kind_check,
    ADD CONSTRAINT ledger_entries_kind_check CHECK (kind IN (
        'deposit_detected', 'deposit_credited', 'deposit_failed', 'lock', 'release', 'transfer', 'trade', 'withdrawal', 'conversion',
        'venue_deposit', 'venue_withdrawal', 'venue_fee', 'venue_fill'));

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
            SELECT CASE WHEN p.account LIKE 'venue:%' THEN 'venue' ELSE p.account END
                || CASE WHEN p.amount::numeric > 0 THEN '+' ELSE '-' END AS posting
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
        WHEN 'venue_deposit' THEN ARRAY['|custody+,venue-']
        WHEN 'venue_withdrawal' THEN ARRAY['|custody-,venue+']
        WHEN 'venue_fee' THEN ARRAY['|inventory-,venue+']
        WHEN 'venue_fill' THEN ARRAY['|inventory+,inventory-,venue+,venue-']
        WHEN 'conversion' THEN ARRAY['CONVERSION_CREDIT,CONVERSION_DEBIT,LOCK|custody+,custody-',
            'CONVERSION_CREDIT,CONVERSION_DEBIT,LOCK|custody+,custody-,fees+',
            'CONVERSION_DEBIT,LOCK|custody+,custody-,fees+']
        ELSE ARRAY[]::text[] END) THEN
        RAISE EXCEPTION 'ledger entry % of kind % has the shape %, which no operation of the ledger writes', entry, entry_kind, shape
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;

-- A lockdown of otc_routing stops the placement of routed orders (#387);
-- the orders already placed are followed to the end.
ALTER TABLE platform_controls
    DROP CONSTRAINT platform_controls_feature_check,
    ADD CONSTRAINT platform_controls_feature_check CHECK (feature IN ('sign_up', 'sign_in', 'profile_changes', 'deposits', 'withdrawals', 'trading', 'api_keys', 'websocket', 'read_only', 'scheduler', 'outbox', 'market_data', 'otc_routing'));

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
            'otc_trades', 'otc_house_positions',
            'venue_reservations', 'venue_holds',
            'otc_routing_rules', 'otc_routing_usage',
            'otc_routed_orders'));
