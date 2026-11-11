-- Recovery codes of users (#307): ten single-use codes, shown once when the
-- account password is set. One of them, with a code sent to a contact,
-- resets a forgotten password. Only their hashes are stored. A code is
-- retired when it is used, or when the user generates new ones.
CREATE TABLE user_recovery_codes (
    id BIGSERIAL PRIMARY KEY,
    user_hk UUID NOT NULL REFERENCES users (hk),
    code_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retired_at TIMESTAMPTZ,
    retired_reason TEXT,
    CONSTRAINT user_recovery_codes_user_hash_key UNIQUE (user_hk, code_hash),
    CONSTRAINT user_recovery_codes_retired_check CHECK ((retired_at IS NULL) = (retired_reason IS NULL)),
    CONSTRAINT user_recovery_codes_reason_check CHECK (retired_reason IN ('used', 'replaced'))
);

CREATE INDEX user_recovery_codes_active_idx ON user_recovery_codes (user_hk) WHERE retired_at IS NULL;
