-- The sealed secrets cannot go back into secret_hash without decrypting
-- them, so rolling back requires the API keys to be deleted first.
DO $$
DECLARE
    existing BIGINT;
BEGIN
    SELECT COUNT(*) INTO existing FROM api_keys;
    IF existing > 0 THEN
        RAISE EXCEPTION 'rolling back migration 16: % API key row(s) exist; delete them first, then run "migrate force 16" and roll back again', existing;
    END IF;
END
$$;

ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_verification_material;
ALTER TABLE api_keys
    DROP COLUMN IF EXISTS secret_sealed,
    DROP COLUMN IF EXISTS ed25519_public_key;
ALTER TABLE api_keys ADD COLUMN secret_hash TEXT NOT NULL;
