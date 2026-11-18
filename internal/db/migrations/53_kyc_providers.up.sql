-- The KYC providers (#72): an application may be verified by Sumsub, which
-- names it by its id, and is known here by Sumsub's id of its applicant,
-- one application per applicant.
ALTER TABLE kyc_applications
    DROP CONSTRAINT kyc_applications_provider_check,
    ADD CONSTRAINT kyc_applications_provider_check CHECK (provider IN ('manual', 'sumsub')),
    ADD CONSTRAINT kyc_applications_provider_ref_check CHECK ((provider = 'manual') = (provider_ref_id IS NULL));
CREATE UNIQUE INDEX kyc_applications_provider_ref ON kyc_applications (provider, provider_ref_id) WHERE provider_ref_id IS NOT NULL;
