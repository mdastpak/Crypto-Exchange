-- The plaintext must hold every value before the ciphertext goes: a value
-- stored only encrypted would be lost. This release always writes the
-- plaintext too, so only a later one leaves it empty, and its decrypt
-- command fills it again.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM user_contacts WHERE value IS NULL)
       OR EXISTS (SELECT 1 FROM users WHERE (first_name IS NULL AND first_name_enc IS NOT NULL)
                                         OR (last_name IS NULL AND last_name_enc IS NOT NULL)
                                         OR (display_name IS NULL AND display_name_enc IS NOT NULL)
                                         OR (last_login_ip IS NULL AND last_login_ip_enc IS NOT NULL)) THEN
        RAISE EXCEPTION 'some personal fields are stored only encrypted; run "admin fields decrypt" before this down migration';
    END IF;
END $$;

DROP TRIGGER IF EXISTS users_plaintext_written ON users;
DROP FUNCTION IF EXISTS users_plaintext_written();
DROP TRIGGER IF EXISTS user_contacts_plaintext_written ON user_contacts;
DROP FUNCTION IF EXISTS user_contacts_plaintext_written();

ALTER TABLE users
    DROP COLUMN IF EXISTS last_login_ip_enc,
    DROP COLUMN IF EXISTS display_name_enc,
    DROP COLUMN IF EXISTS last_name_enc,
    DROP COLUMN IF EXISTS first_name_enc;

DROP INDEX IF EXISTS user_contacts_value_bidx_unique;

ALTER TABLE user_contacts
    DROP CONSTRAINT IF EXISTS user_contacts_value_indexed,
    DROP CONSTRAINT IF EXISTS user_contacts_value_stored,
    DROP COLUMN IF EXISTS value_bidx,
    DROP COLUMN IF EXISTS value_enc,
    ALTER COLUMN value SET NOT NULL;
