-- The sweeps of the deposit addresses (#68): the server asks the signer to
-- move an asset from a deposit address to the hot wallet, or to the cold
-- storage, once its deposits are credited. A sweep is named at the signer
-- by its key, and a deposit address has one pending sweep at a time, as a
-- sweep of its TRX would take the fee that a token sweep was topped up
-- with. Rows are sealed from their first version: a sweep marked completed
-- in SQL would hide funds left on an address.
CREATE TABLE sweeps (
    id UUID PRIMARY KEY,
    deposit_address_id UUID NOT NULL REFERENCES deposit_addresses (id),
    currency_hk UUID NOT NULL REFERENCES currencies (hk),
    sweep_key TEXT NOT NULL UNIQUE CONSTRAINT sweeps_key_check CHECK (sweep_key ~ '^[A-Za-z0-9:_-]{1,128}$'),
    status TEXT NOT NULL CONSTRAINT sweeps_status_check CHECK (status IN ('pending', 'completed', 'failed')),
    amount TEXT CONSTRAINT sweeps_amount_check CHECK (amount ~ '^[0-9]{1,78}$'),
    to_address TEXT CONSTRAINT sweeps_to_check CHECK (length(to_address) BETWEEN 1 AND 100),
    top_up_tx_id TEXT CONSTRAINT sweeps_top_up_check CHECK (top_up_tx_id ~ '^[0-9a-f]{64}$'),
    tx_id TEXT CONSTRAINT sweeps_tx_check CHECK (tx_id ~ '^[0-9a-f]{64}$'),
    reason TEXT CONSTRAINT sweeps_reason_check CHECK (length(reason) BETWEEN 1 AND 500),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER
);
CREATE UNIQUE INDEX sweeps_one_pending ON sweeps (deposit_address_id) WHERE status = 'pending';
CREATE INDEX sweeps_address_currency ON sweeps (deposit_address_id, currency_hk, created_at DESC);

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
            'deposit_addresses', 'deposits', 'chain_cursors',
            'sweeps'));
