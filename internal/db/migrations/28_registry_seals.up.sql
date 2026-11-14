-- Seals of the networks of the currencies and of the markets (#57, #139).
--
-- Custody credits a deposit by the contract of its network, scales it by
-- the network's decimals, and pays a withdrawal within its limits and fee;
-- a market trades by its status, rules, and fees. A row changed in SQL,
-- such as a contract swapped for another token's, must not be acted on, so
-- every write seals the row and every read checks it.
--
-- currency_networks has a key of two columns; a seal names its row by one,
-- so the table gains id. The rows of before have no seal: the verifier
-- seals them in report mode (docs/data-protection.md#modes).
ALTER TABLE currency_networks
    ADD COLUMN id uuid NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER,
    ADD CONSTRAINT currency_networks_id_key UNIQUE (id);

ALTER TABLE markets
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
            'user_anti_phishing_codes', 'sms_routing', 'currency_networks', 'markets'));
