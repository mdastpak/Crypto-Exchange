DELETE FROM integrity_findings WHERE table_name IN ('deposits', 'chain_cursors');
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
            'kyc_applications', 'kyc_documents',
            'deposit_addresses'));

DROP TABLE chain_cursors;

-- The deposits of the watcher have no meaning in the schema of before; the
-- ledger keeps what was credited.
DELETE FROM deposits;
DROP INDEX IF EXISTS deposits_detected;
DROP INDEX IF EXISTS deposits_user;
DROP INDEX IF EXISTS deposits_network_tx_log;
ALTER TABLE deposits
    DROP CONSTRAINT deposits_hold_check,
    DROP CONSTRAINT deposits_credited_check,
    DROP CONSTRAINT deposits_amount_check,
    DROP COLUMN seal_key_version,
    DROP COLUMN row_seal,
    DROP COLUMN row_version,
    DROP COLUMN hold_reason,
    DROP COLUMN from_address,
    DROP COLUMN user_id,
    DROP COLUMN log_index,
    DROP COLUMN network,
    ADD CONSTRAINT uq_deposits_tx_currency UNIQUE (tx_hash, currency_hk);
