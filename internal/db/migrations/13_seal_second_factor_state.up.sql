-- Row seals for the state of second factors and recoveries (#337): the
-- authenticator apps of users, the account recoveries, and the step-ups of
-- sessions. Changing them in SQL turned an authenticator back on, let a
-- recovery skip its proofs and its wait, or let a session run the
-- operations that a factor protects without proving it again.
--
-- The rows written before this migration have no seal. Roll it out with
-- keys.row_seals.mode report, as docs/data-protection.md describes: the
-- verifier seals them, and enforce mode can follow.
ALTER TABLE user_totp
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER;

ALTER TABLE account_recoveries
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER;

ALTER TABLE session_step_ups
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER;

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups'));
