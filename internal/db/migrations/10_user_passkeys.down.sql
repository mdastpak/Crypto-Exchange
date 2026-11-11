DELETE FROM integrity_findings WHERE table_name = 'user_passkeys';
ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords'));
DROP TABLE IF EXISTS user_passkeys;
