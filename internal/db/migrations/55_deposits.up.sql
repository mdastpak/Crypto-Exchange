-- The deposits on chain (#68), as the watcher of a network finds them to
-- the deposit addresses. A deposit is a transfer of a transaction, by its
-- log index, once per network: a duplicate notification, or a block read
-- again after a reorg, finds it recorded. It is DETECTED in its block, and
-- credited once its block is confirmed and still canonical, or FAILED when
-- a reorg drops its transaction. A confirmed deposit that is not credited
-- has a hold: below the minimum, deposits closed, the user's deposits
-- restricted, or the AML rules; an administrator credits it after review.
-- Rows are sealed from their first version: a deposit raised or moved to
-- another user in SQL is refused before it is credited.
DELETE FROM deposits;
ALTER TABLE deposits
    DROP CONSTRAINT uq_deposits_tx_currency,
    ADD COLUMN network TEXT NOT NULL CONSTRAINT deposits_network_check CHECK (network ~ '^[a-z][a-z0-9_]{1,29}$'),
    ADD COLUMN log_index INTEGER NOT NULL CONSTRAINT deposits_log_index_check CHECK (log_index >= -1),
    ADD COLUMN user_id UUID NOT NULL REFERENCES users (hk),
    ADD COLUMN from_address TEXT CONSTRAINT deposits_from_address_check CHECK (length(from_address) BETWEEN 1 AND 200),
    ADD COLUMN hold_reason TEXT CONSTRAINT deposits_hold_reason_check
        CHECK (hold_reason IN ('below_minimum', 'deposits_closed', 'restricted', 'aml')),
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER,
    ADD CONSTRAINT deposits_amount_check CHECK (amount::numeric > 0),
    ADD CONSTRAINT deposits_credited_check CHECK ((status = 'CREDITED') = (credited_at IS NOT NULL)),
    ADD CONSTRAINT deposits_hold_check CHECK ((hold_reason IS NULL) OR status IN ('CONFIRMED', 'FAILED'));
CREATE UNIQUE INDEX deposits_network_tx_log ON deposits (network, tx_hash, log_index);
CREATE INDEX deposits_user ON deposits (user_id, detected_at DESC);
CREATE INDEX deposits_detected ON deposits (network, block_height) WHERE status = 'DETECTED';

-- How far the watcher of each network has read, and the id of that block,
-- which the next block's parent must be: one that is not was reorganized.
-- Sealed: a cursor moved in SQL would skip blocks, and the deposits in
-- them.
CREATE TABLE chain_cursors (
    network TEXT PRIMARY KEY CONSTRAINT chain_cursors_network_check CHECK (network ~ '^[a-z][a-z0-9_]{1,29}$'),
    block_number BIGINT NOT NULL CONSTRAINT chain_cursors_block_number_check CHECK (block_number >= 0),
    block_id TEXT NOT NULL CONSTRAINT chain_cursors_block_id_check CHECK (length(block_id) BETWEEN 1 AND 100),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER
);

-- The new sealed tables may have findings; every table of before stays.
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
            'deposit_addresses', 'deposits', 'chain_cursors'));
