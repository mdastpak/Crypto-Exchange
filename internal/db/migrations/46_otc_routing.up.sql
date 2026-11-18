-- The routing rules of the OTC trades (#386): the policy, set by the
-- administrators, that picks the exchange's own inventory or an external
-- venue for the liquidity of each trade. Each change is a new version of
-- the whole set; the highest version applies on every node within its
-- cache window. A version is never changed or deleted after its seal (#248).
CREATE TABLE otc_routing_rules (
    version bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- The rules (routing.Rule), as JSON.
    rules jsonb NOT NULL CONSTRAINT otc_routing_rules_rules_check CHECK (jsonb_typeof(rules) = 'array'),
    reason text NOT NULL CONSTRAINT otc_routing_rules_reason_check CHECK (length(reason) BETWEEN 1 AND 1000),
    created_by uuid NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER
);

-- The notional that the routed orders took from each venue per day, in a
-- quote currency, which the rules' daily limits check. The routed orders
-- (#387) add to it in their transaction.
CREATE TABLE otc_routing_usage (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    venue text NOT NULL CONSTRAINT otc_routing_usage_venue_check CHECK (venue ~ '^[a-z][a-z0-9_]{0,29}$'),
    day date NOT NULL,
    currency_hk uuid NOT NULL REFERENCES currencies (hk),
    notional exact_decimal NOT NULL CONSTRAINT otc_routing_usage_notional_check CHECK (notional::numeric >= 0),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT otc_routing_usage_key UNIQUE (venue, day, currency_hk)
);

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
            'otc_routing_rules', 'otc_routing_usage'));
