DELETE FROM integrity_findings WHERE table_name IN ('currency_migrations', 'currency_delistings');
ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods',
            'user_anti_phishing_codes', 'sms_routing', 'currency_networks', 'markets', 'withdrawals', 'integrity_findings',
            'withdrawal_payouts', 'deny_links', 'price_overrides'));

ALTER TABLE currency_delistings DROP COLUMN row_version, DROP COLUMN row_seal, DROP COLUMN seal_key_version;
ALTER TABLE currency_migrations DROP COLUMN row_version, DROP COLUMN row_seal, DROP COLUMN seal_key_version;
