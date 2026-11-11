-- The account password of a user (#307): a second step after the sign-in
-- code, never a replacement for it. The hash is an Argon2id string. Its row
-- is sealed like the account (#139), so that a hash written in SQL is
-- detected. failed_attempts and locked_until count the wrong passwords, for
-- every instance, and are outside the seal.
CREATE TABLE user_passwords (
    user_hk UUID PRIMARY KEY REFERENCES users (hk),
    password_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    changed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    failed_attempts INTEGER NOT NULL DEFAULT 0,
    locked_until TIMESTAMPTZ,
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT user_passwords_hash_check CHECK (password_hash LIKE '$argon2id$%'),
    CONSTRAINT user_passwords_failed_attempts_check CHECK (failed_attempts >= 0)
);

-- A finding may name a row of the new table.
ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords'));
