-- The audit of the books (#60).
--
-- A snapshot of a balance is of one version of it: the snapshot job may run
-- on several instances at once, and writes each version once.
CREATE UNIQUE INDEX balance_snapshots_balance_version ON balance_snapshots (balance_id, version);

-- Row seals for the balances (#139): a balance changed in SQL fails its
-- seal on read, and the ledger refuses to move it in enforce mode, besides
-- the reconciliation that replays it from its changes. Nothing wrote the
-- balances before the ledger, so every row is sealed from its first write.
ALTER TABLE balances
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER;

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances'));
