-- The state of the external venues that cover the OTC trades (#385): what
-- we hold at each, its markets, and its networks, from the snapshots that
-- a job of cmd/market-worker takes under a lease; the reservations of the
-- balances that routed orders plan to spend; and the holds that a
-- difference between a venue and the books puts on a currency of it.
--
-- A snapshot replaces the rows of its kind and venue: a row that the venue
-- no longer reports is zeroed or disabled, never deleted (#248). Every row
-- carries the time of its snapshot; the router treats one older than
-- venues.state.max_age_seconds as unknown.
CREATE TABLE venue_balances (
    venue text NOT NULL CONSTRAINT venue_balances_venue_check CHECK (venue ~ '^[a-z][a-z0-9_]{0,29}$'),
    asset text NOT NULL CONSTRAINT venue_balances_asset_check CHECK (length(asset) BETWEEN 1 AND 30),
    free exact_decimal NOT NULL CONSTRAINT venue_balances_free_check CHECK (free::numeric >= 0),
    locked exact_decimal NOT NULL CONSTRAINT venue_balances_locked_check CHECK (locked::numeric >= 0),
    taken_at TIMESTAMPTZ NOT NULL,
    -- The reconciliations in a row that found this balance apart from the
    -- books; a hold opens at venues.state.confirm_runs, so that a move in
    -- flight between the snapshot and its entry holds nothing.
    mismatch_runs integer NOT NULL DEFAULT 0 CONSTRAINT venue_balances_mismatch_runs_check CHECK (mismatch_runs >= 0),
    PRIMARY KEY (venue, asset)
);

CREATE TABLE venue_instruments (
    venue text NOT NULL,
    symbol text NOT NULL CONSTRAINT venue_instruments_symbol_check CHECK (length(symbol) BETWEEN 1 AND 40),
    base text NOT NULL,
    quote text NOT NULL,
    trading boolean NOT NULL,
    tick_size exact_decimal NOT NULL DEFAULT 0,
    lot_size exact_decimal NOT NULL DEFAULT 0,
    min_quantity exact_decimal NOT NULL DEFAULT 0,
    min_notional exact_decimal NOT NULL DEFAULT 0,
    taken_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (venue, symbol)
);

CREATE TABLE venue_networks (
    venue text NOT NULL,
    asset text NOT NULL,
    network text NOT NULL CONSTRAINT venue_networks_network_check CHECK (length(network) BETWEEN 1 AND 40),
    deposit_enabled boolean NOT NULL,
    withdraw_enabled boolean NOT NULL,
    withdraw_fee exact_decimal NOT NULL DEFAULT 0,
    withdraw_min exact_decimal NOT NULL DEFAULT 0,
    taken_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (venue, asset, network)
);

-- The last snapshot of each kind of a venue, and the last failure, for
-- the health of the venue.
CREATE TABLE venue_snapshots (
    venue text NOT NULL,
    kind text NOT NULL CONSTRAINT venue_snapshots_kind_check CHECK (kind IN ('balances', 'instruments', 'networks', 'reconciliation')),
    taken_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    -- The kind of the last failure (venues.Kind), never the venue's text,
    -- which may carry an address or an amount.
    failure text,
    PRIMARY KEY (venue, kind)
);

-- A reservation of a venue balance by a routed order: the balances that
-- the router may spend are the free balance of the snapshot less the open
-- reservations, read under the lock of the balance's row, so two nodes
-- never plan to spend the same funds. A reservation ends with released_at,
-- never by a delete (#248).
CREATE TABLE venue_reservations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    venue text NOT NULL,
    asset text NOT NULL,
    amount exact_decimal NOT NULL CONSTRAINT venue_reservations_amount_check CHECK (amount::numeric > 0),
    reference_type text NOT NULL,
    reference_id text NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ,
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    -- No foreign key to venue_balances: an asset of a venue is the venue's
    -- name of one of our currencies (venues.<name>.assets), not a second
    -- registry of assets (#309). A reservation is made under the lock of
    -- its balance's row, which must exist.
    CONSTRAINT venue_reservations_reference_key UNIQUE (venue, asset, reference_type, reference_id)
);

CREATE INDEX venue_reservations_open ON venue_reservations (venue, asset) WHERE released_at IS NULL;

-- A hold of a currency of a venue, opened by a reconciliation whose
-- difference between the venue and the books is beyond the tolerance. The
-- currency is not routable at the venue until an operator clears it.
CREATE TABLE venue_holds (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    venue text NOT NULL,
    asset text NOT NULL,
    currency_hk uuid NOT NULL REFERENCES currencies (hk),
    -- What the venue reported holding, what the books say it holds, and
    -- reported - booked.
    reported exact_decimal NOT NULL,
    booked exact_decimal NOT NULL,
    difference exact_decimal NOT NULL,
    detected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    cleared_at TIMESTAMPTZ,
    cleared_by uuid,
    clear_reason text CONSTRAINT venue_holds_clear_reason_check CHECK (length(clear_reason) <= 1000),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT venue_holds_cleared_check CHECK ((cleared_at IS NULL) = (cleared_by IS NULL))
);

-- One open hold per currency of a venue.
CREATE UNIQUE INDEX venue_holds_open ON venue_holds (venue, asset) WHERE cleared_at IS NULL;

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
            'venue_reservations', 'venue_holds'));

-- The house accounts at the venues (#385): venue:<name> per currency holds
-- the negative of what we hold at the venue, as custody does of what
-- custody holds. A deposit of ours to a venue moves custody to it, a
-- withdrawal from it back, and a fee that the venue charges takes from the
-- inventory, the exchange's own funds.
ALTER TABLE ledger_postings
    DROP CONSTRAINT ledger_postings_account_check,
    ADD CONSTRAINT ledger_postings_account_check CHECK (
        account IN ('custody', 'fees', 'inventory', 'deposits_pending') OR account ~ '^venue:[a-z][a-z0-9_]{0,29}$');

ALTER TABLE ledger_entries
    DROP CONSTRAINT ledger_entries_kind_check,
    ADD CONSTRAINT ledger_entries_kind_check CHECK (kind IN (
        'deposit_detected', 'deposit_credited', 'deposit_failed', 'lock', 'release', 'transfer', 'trade', 'withdrawal', 'conversion',
        'venue_deposit', 'venue_withdrawal', 'venue_fee'));

-- The shapes of the entries of the venues; an account venue:<name> counts
-- as venue.
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
        WHEN 'conversion' THEN ARRAY['CONVERSION_CREDIT,CONVERSION_DEBIT,LOCK|custody+,custody-',
            'CONVERSION_CREDIT,CONVERSION_DEBIT,LOCK|custody+,custody-,fees+',
            'CONVERSION_DEBIT,LOCK|custody+,custody-,fees+']
        ELSE ARRAY[]::text[] END) THEN
        RAISE EXCEPTION 'ledger entry % of kind % has the shape %, which no operation of the ledger writes', entry, entry_kind, shape
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;
