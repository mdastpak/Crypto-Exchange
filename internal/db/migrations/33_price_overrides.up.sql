-- The prices that administrators set for a market until a time, in place
-- of the aggregated reference (#59), for when the providers are wrong or
-- down. Every instance of the server reads the overrides in force every
-- second, and the readers of the prices take one first. A row is sealed
-- from the first one (#139): an override set, moved, or lengthened in SQL
-- is not served.
CREATE TABLE price_overrides (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The market, BASE/QUOTE, as the price engine names it.
    market text NOT NULL CONSTRAINT price_overrides_market_check CHECK (market ~ '^[A-Z0-9]{1,15}/[A-Z0-9]{1,15}$'),
    bid exact_decimal NOT NULL,
    ask exact_decimal NOT NULL,
    mid exact_decimal NOT NULL,
    reason text NOT NULL CONSTRAINT price_overrides_reason_check CHECK (length(reason) BETWEEN 1 AND 500),
    created_by uuid NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    -- When it ended before its expiry, and by whom; an override that
    -- expired is closed with ended_at at its expiry when another is set.
    ended_at TIMESTAMPTZ,
    ended_by uuid,
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT price_overrides_prices_check CHECK (
        bid::numeric > 0 AND bid::numeric <= mid::numeric AND mid::numeric <= ask::numeric),
    CONSTRAINT price_overrides_expiry_check CHECK (expires_at > created_at)
);

-- One open override per market.
CREATE UNIQUE INDEX price_overrides_open ON price_overrides (market) WHERE ended_at IS NULL;
CREATE INDEX price_overrides_created ON price_overrides (created_at DESC);

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods',
            'user_anti_phishing_codes', 'sms_routing', 'currency_networks', 'markets', 'withdrawals', 'integrity_findings',
            'withdrawal_payouts', 'deny_links', 'price_overrides'));
