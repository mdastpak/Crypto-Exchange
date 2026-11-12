-- The findings of the four tables go with their seals.
DELETE FROM integrity_findings WHERE table_name IN ('admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes');
ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups'));

ALTER TABLE user_recovery_codes
    DROP COLUMN IF EXISTS seal_key_version,
    DROP COLUMN IF EXISTS row_seal,
    DROP COLUMN IF EXISTS row_version;

ALTER TABLE platform_controls
    DROP COLUMN IF EXISTS seal_key_version,
    DROP COLUMN IF EXISTS row_seal,
    DROP COLUMN IF EXISTS row_version;

ALTER TABLE user_restrictions
    DROP COLUMN IF EXISTS seal_key_version,
    DROP COLUMN IF EXISTS row_seal,
    DROP COLUMN IF EXISTS row_version;

ALTER TABLE admin_sessions
    DROP COLUMN IF EXISTS seal_key_version,
    DROP COLUMN IF EXISTS row_seal,
    DROP COLUMN IF EXISTS row_version;
