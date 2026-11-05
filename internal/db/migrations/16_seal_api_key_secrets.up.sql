-- Migration 16 replaces the plaintext API key secrets of migration 8 (#45).
-- secret_hash held the secret itself. An HMAC key now stores its secret
-- encrypted with a field-enc data key, bound to its key ID, because the
-- server needs the secret to verify signatures. An Ed25519 key stores only
-- the client's public key.

-- Preflight: rows written before hold plaintext secrets, which cannot be
-- converted without exposing them. The API does not issue API keys yet.
DO $$
DECLARE
    existing BIGINT;
BEGIN
    SELECT COUNT(*) INTO existing FROM api_keys;
    IF existing > 0 THEN
        RAISE EXCEPTION 'migration 16: % API key row(s) store their secret in plaintext; delete them and issue new keys, then run "migrate force 15" and start the server again', existing;
    END IF;
END
$$;

ALTER TABLE api_keys DROP COLUMN secret_hash;
ALTER TABLE api_keys
    ADD COLUMN secret_sealed BYTEA,
    ADD COLUMN ed25519_public_key BYTEA;

-- Each key type keeps exactly what verifying its requests needs. The IS NOT
-- NULL tests matter: a CHECK that evaluates to NULL passes.
ALTER TABLE api_keys ADD CONSTRAINT api_keys_verification_material CHECK (
    (key_type = 'HMAC' AND secret_sealed IS NOT NULL AND ed25519_public_key IS NULL)
    OR (key_type = 'ED25519' AND ed25519_public_key IS NOT NULL AND octet_length(ed25519_public_key) = 32
        AND secret_sealed IS NULL)
);
