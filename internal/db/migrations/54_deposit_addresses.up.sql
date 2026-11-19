-- The deposit addresses of the custody (#68). A user has one address per
-- network, which every currency of the network shares, such as TRX and
-- USDT on TRON: currency_hk is NULL for it. The address is derived from
-- the extended public key of the account, by its index, so the server
-- holds no private key; network and index name it once, and the address
-- belongs to one user. The row is sealed from its first version: an
-- address swapped in SQL would credit another user's deposits, or the
-- deposits of an address that the signer does not hold.
DELETE FROM deposit_addresses;
ALTER TABLE deposit_addresses
    DROP CONSTRAINT uq_deposit_addresses_user_currency_address,
    ALTER COLUMN currency_hk DROP NOT NULL,
    ADD COLUMN network TEXT NOT NULL CONSTRAINT deposit_addresses_network_check CHECK (network ~ '^[a-z][a-z0-9_]{1,29}$'),
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER,
    ADD CONSTRAINT deposit_addresses_index_check CHECK (index >= 0),
    ADD CONSTRAINT deposit_addresses_derived_check CHECK ((derivation_path IS NULL) = (index IS NULL));
CREATE UNIQUE INDEX deposit_addresses_network_address ON deposit_addresses (network, address);
CREATE UNIQUE INDEX deposit_addresses_network_index ON deposit_addresses (network, index) WHERE index IS NOT NULL;
CREATE UNIQUE INDEX deposit_addresses_user_network ON deposit_addresses (user_id, network) WHERE sub_account_id IS NULL AND active;

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
            'sanctions_lists',
            'kyc_applications', 'kyc_documents',
            'deposit_addresses'));
