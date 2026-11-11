-- Passkeys of users (#307): WebAuthn credentials, such as Face ID, a
-- fingerprint, Windows Hello, or a security key, that prove the second step
-- instead of the account password. credential is the public part, as JSON.
-- The row is sealed like the account (#139), so that a credential written
-- in SQL is detected and proves nothing. sign_count and last_used_at change
-- at each use, and are outside the seal.
CREATE TABLE user_passkeys (
    id UUID PRIMARY KEY,
    user_hk UUID NOT NULL REFERENCES users (hk),
    credential_id BYTEA NOT NULL,
    credential TEXT NOT NULL,
    name TEXT NOT NULL,
    sign_count BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    -- Set when the user removed the passkey. The runtime role deletes
    -- nothing here (#248).
    removed_at TIMESTAMPTZ,
    row_version BIGINT NOT NULL DEFAULT 0,
    row_seal BYTEA,
    seal_key_version INTEGER,
    CONSTRAINT user_passkeys_name_check CHECK (name <> '' AND char_length(name) <= 64),
    CONSTRAINT user_passkeys_sign_count_check CHECK (sign_count >= 0)
);

-- A credential belongs to one account.
CREATE UNIQUE INDEX user_passkeys_credential_key ON user_passkeys (credential_id) WHERE removed_at IS NULL;
CREATE INDEX user_passkeys_user_idx ON user_passkeys (user_hk) WHERE removed_at IS NULL;

-- A finding may name a row of the new table.
ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys'));
