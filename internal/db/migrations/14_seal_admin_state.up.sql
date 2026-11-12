-- Row seals for the rest of the state that decides access (#337): the
-- sessions of administrators, the compliance restrictions of users, the
-- maintenance windows and lockdowns of the platform, and the recovery codes
-- of users. Changing them in SQL made a revoked admin token work again,
-- ended a freeze or a lockdown without its audited call, let one
-- administrator lift a lockdown alone, or gave an account a recovery code
-- that its owner never saw.
--
-- The rows written before this migration have no seal. Roll it out with
-- keys.row_seals.mode report, as docs/data-protection.md describes: the
-- verifier seals them, and enforce mode can follow.
ALTER TABLE admin_sessions
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER;

ALTER TABLE user_restrictions
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER;

ALTER TABLE platform_controls
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER;

ALTER TABLE user_recovery_codes
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER;

ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes'));
