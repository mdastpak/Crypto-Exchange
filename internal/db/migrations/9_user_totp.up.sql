-- Authenticator apps (TOTP) as a second step of users (#307), usable
-- instead of the account password. The secret is sealed with the
-- data-protection keys, bound to the user, so a row written outside the
-- backend holds no secret that it can open.
CREATE TABLE user_totp (
    user_hk UUID PRIMARY KEY REFERENCES users (hk),
    secret_sealed BYTEA NOT NULL,
    -- NULL until the user proved a code of the secret.
    confirmed_at TIMESTAMPTZ,
    -- The time step of the last code that was accepted: a code is used once.
    last_step BIGINT NOT NULL DEFAULT 0,
    failed_attempts INTEGER NOT NULL DEFAULT 0,
    locked_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Set when the user turned the authenticator off. The runtime role
    -- deletes nothing here (#248); the secret is emptied instead.
    removed_at TIMESTAMPTZ,
    CONSTRAINT user_totp_failed_attempts_check CHECK (failed_attempts >= 0)
);
