-- The sanctions lists (#73). A job downloads the OFAC list on a schedule
-- and keeps each version that differs from the last, compressed, with the
-- digest of what was downloaded; checked_at is when a download last found
-- it current. Every instance loads the newest version and screens against
-- it in memory. A row is sealed from its first version, and the seal covers
-- the body by its SHA-256, computed in SQL: an entry removed in SQL, which
-- would let a sanctioned address through, fails the load, and the
-- screening holds every operation it screens for review until a version is
-- downloaded again.
CREATE TABLE sanctions_lists (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider TEXT NOT NULL CONSTRAINT sanctions_lists_provider_check CHECK (provider IN ('ofac')),
    source_digest BYTEA NOT NULL,
    body BYTEA NOT NULL,
    entry_count INTEGER NOT NULL CONSTRAINT sanctions_lists_entry_count_check CHECK (entry_count > 0),
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    checked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER
);
CREATE INDEX sanctions_lists_latest ON sanctions_lists (provider, id DESC);

-- The new sealed table may have findings; every table of before stays.
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
            'otc_routed_orders',
            'user_national_ids', 'identity_checks',
            'aml_rules', 'aml_rule_history', 'aml_events', 'aml_alerts',
            'sanctions_lists'));
