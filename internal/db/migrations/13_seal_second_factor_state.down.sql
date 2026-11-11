-- The findings of the three tables go with their seals.
DELETE FROM integrity_findings WHERE table_name IN ('user_totp', 'account_recoveries', 'session_step_ups');
ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys'));

ALTER TABLE session_step_ups
    DROP COLUMN IF EXISTS seal_key_version,
    DROP COLUMN IF EXISTS row_seal,
    DROP COLUMN IF EXISTS row_version;

ALTER TABLE account_recoveries
    DROP COLUMN IF EXISTS seal_key_version,
    DROP COLUMN IF EXISTS row_seal,
    DROP COLUMN IF EXISTS row_version;

ALTER TABLE user_totp
    DROP COLUMN IF EXISTS seal_key_version,
    DROP COLUMN IF EXISTS row_seal,
    DROP COLUMN IF EXISTS row_version;
