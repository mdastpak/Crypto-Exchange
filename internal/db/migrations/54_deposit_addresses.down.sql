DELETE FROM integrity_findings WHERE table_name = 'deposit_addresses';
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
            'sanctions_lists',
            'kyc_applications', 'kyc_documents'));

-- The addresses of a network mean nothing to the schema of before, which
-- names a currency per address: they are removed; the keys derive them
-- again.
DELETE FROM deposit_addresses;
DROP INDEX IF EXISTS deposit_addresses_user_network;
DROP INDEX IF EXISTS deposit_addresses_network_index;
DROP INDEX IF EXISTS deposit_addresses_network_address;
ALTER TABLE deposit_addresses
    DROP CONSTRAINT deposit_addresses_derived_check,
    DROP CONSTRAINT deposit_addresses_index_check,
    DROP COLUMN seal_key_version,
    DROP COLUMN row_seal,
    DROP COLUMN row_version,
    DROP COLUMN network,
    ALTER COLUMN currency_hk SET NOT NULL,
    ADD CONSTRAINT uq_deposit_addresses_user_currency_address UNIQUE (user_id, sub_account_id, currency_hk, address);
