DELETE FROM integrity_findings WHERE table_name IN ('user_national_ids', 'identity_checks');
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

-- A destination still pending its ownership check was never usable.
DELETE FROM withdrawal_destinations WHERE ownership_pending_since IS NOT NULL;
DROP INDEX IF EXISTS withdrawal_destinations_ownership_due;
ALTER TABLE withdrawal_destinations
    DROP CONSTRAINT withdrawal_destinations_ownership_check,
    DROP CONSTRAINT withdrawal_destinations_fiat_check,
    ADD CONSTRAINT withdrawal_destinations_fiat_check CHECK (kind = 'crypto' OR (memo_enc IS NULL AND verified_by IS NOT NULL)),
    DROP COLUMN ownership_next_check,
    DROP COLUMN ownership_attempts,
    DROP COLUMN ownership_pending_since;

DROP TABLE IF EXISTS identity_checks;

DROP INDEX IF EXISTS user_national_ids_bidx;
ALTER TABLE user_national_ids
    DROP COLUMN seal_key_version,
    DROP COLUMN row_seal,
    DROP COLUMN row_version,
    DROP COLUMN birth_date_sealed,
    DROP COLUMN national_id_bidx;
