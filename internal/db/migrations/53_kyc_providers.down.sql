-- An application verified by a provider has no meaning in the schema of
-- before, and changing it would break its seal: the migration stops.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM kyc_applications WHERE provider <> 'manual') THEN
        RAISE EXCEPTION 'kyc_applications has applications verified by a provider; they cannot be migrated down';
    END IF;
END
$$;
DROP INDEX IF EXISTS kyc_applications_provider_ref;
ALTER TABLE kyc_applications
    DROP CONSTRAINT kyc_applications_provider_ref_check,
    DROP CONSTRAINT kyc_applications_provider_check,
    ADD CONSTRAINT kyc_applications_provider_check CHECK (provider IN ('manual'));
