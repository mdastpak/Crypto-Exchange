-- The rows that decide how much the ledger credits the holders of a
-- currency that changes or goes are sealed (#451, #139): a migration of
-- currencies, with its sources and their ratios in its seal, and a
-- delisting, with its settlement price. The rows that exist are sealed by
-- the verifier; the conversions convert nothing at a row that fails its
-- check, or that is not sealed yet.
ALTER TABLE currency_migrations
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER;
ALTER TABLE currency_delistings
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER;

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods',
            'user_anti_phishing_codes', 'sms_routing', 'currency_networks', 'markets', 'withdrawals', 'integrity_findings',
            'withdrawal_payouts', 'deny_links', 'price_overrides',
            'currency_migrations', 'currency_delistings'));
