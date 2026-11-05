-- Data-protection keys (#138). Each row holds one data key, wrapped by the
-- key-encryption key (KEK) of a key management service, so the database
-- alone never holds a usable key. The wrapped plaintext also names the
-- purpose and the version, so a wrapped key copied to another row fails to
-- load.
CREATE TABLE data_keys (
    purpose     TEXT        NOT NULL CHECK (purpose IN ('row-mac', 'field-enc', 'blind-index')),
    version     INTEGER     NOT NULL CHECK (version > 0),
    -- Identifies the KEK that wrapped the key.
    kek_id      TEXT        NOT NULL,
    wrapped_key BYTEA       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- A retired key is no longer loaded: nothing may still need it.
    retired_at  TIMESTAMPTZ,
    PRIMARY KEY (purpose, version)
);
