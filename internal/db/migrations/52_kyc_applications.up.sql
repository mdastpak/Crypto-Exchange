-- KYC applications with their documents (#72).
--
-- The *_enc columns of the KYC tables were DATE and VARCHAR, which cannot
-- hold ciphertext; they become BYTEA, for the field encryption (#140). No
-- service wrote the KYC tables, so their rows, if any, are removed.
DELETE FROM kyc_documents;
DELETE FROM kyc_applications;
DELETE FROM kyc_profiles;
ALTER TABLE kyc_profiles
    ALTER COLUMN first_name_enc TYPE BYTEA USING NULL,
    ALTER COLUMN last_name_enc TYPE BYTEA USING NULL,
    ALTER COLUMN date_of_birth_enc TYPE BYTEA USING NULL,
    ALTER COLUMN nationality_enc TYPE BYTEA USING NULL,
    ALTER COLUMN country_of_residence_enc TYPE BYTEA USING NULL,
    ALTER COLUMN address_line1_enc TYPE BYTEA USING NULL,
    ALTER COLUMN address_line2_enc TYPE BYTEA USING NULL,
    ALTER COLUMN city_enc TYPE BYTEA USING NULL,
    ALTER COLUMN state_enc TYPE BYTEA USING NULL,
    ALTER COLUMN postal_code_enc TYPE BYTEA USING NULL,
    ALTER COLUMN country_enc TYPE BYTEA USING NULL;

-- An application asks for a level (2, STANDARD), with the form encrypted.
-- It is PENDING while the user adds documents, UNDER_REVIEW once
-- submitted, then APPROVED, which grants the level in the same
-- transaction, or REJECTED with a reason that the user reads; EXPIRED is
-- one that the user withdrew. A user has one open application at a time.
-- The row is sealed from its first version: an application approved in
-- SQL grants nothing, and the level it would have granted is not read
-- from it anyway.
ALTER TABLE kyc_applications
    ALTER COLUMN form_data_enc TYPE BYTEA USING NULL,
    DROP COLUMN documents,
    ADD COLUMN submitted_at TIMESTAMPTZ,
    ADD COLUMN reason TEXT CONSTRAINT kyc_applications_reason_check CHECK (length(reason) BETWEEN 1 AND 500),
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER,
    ADD CONSTRAINT kyc_applications_requested_tier_check CHECK (requested_tier = 'STANDARD'),
    ADD CONSTRAINT kyc_applications_provider_check CHECK (provider IN ('manual')),
    ADD CONSTRAINT kyc_applications_submitted_check CHECK ((status = 'PENDING') = (submitted_at IS NULL) OR status = 'EXPIRED'),
    ADD CONSTRAINT kyc_applications_reviewed_check CHECK ((status IN ('APPROVED', 'REJECTED')) = (reviewed_at IS NOT NULL)),
    ADD CONSTRAINT kyc_applications_rejected_check CHECK ((status = 'REJECTED') = (reason IS NOT NULL)),
    ADD CONSTRAINT kyc_applications_review_notes_check CHECK (length(review_notes) <= 1000);
ALTER TABLE kyc_applications ALTER COLUMN provider SET DEFAULT 'manual';
UPDATE kyc_applications SET provider = 'manual' WHERE provider IS NULL;
ALTER TABLE kyc_applications ALTER COLUMN provider SET NOT NULL;
CREATE UNIQUE INDEX kyc_applications_open ON kyc_applications (user_id) WHERE status IN ('PENDING', 'UNDER_REVIEW');
CREATE INDEX kyc_applications_queue ON kyc_applications (status, submitted_at);

-- A document of an application: its type and kind of file, and where its
-- content is in the object storage, encrypted by the server with the
-- field key, with the SHA-256 of what was stored. The name of the user's
-- file is not kept. The row is sealed from its first version: an object
-- swapped in the storage, or a document moved to another application in
-- SQL, is refused.
ALTER TABLE kyc_documents
    ALTER COLUMN kyc_profile_id DROP NOT NULL,
    ALTER COLUMN file_name DROP NOT NULL,
    ALTER COLUMN extracted_data_enc TYPE BYTEA USING NULL,
    ADD COLUMN application_id UUID NOT NULL REFERENCES kyc_applications (id) ON DELETE CASCADE,
    ADD COLUMN content_digest BYTEA NOT NULL,
    ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN row_seal BYTEA,
    ADD COLUMN seal_key_version INTEGER,
    ADD CONSTRAINT kyc_documents_file_name_check CHECK (file_name IS NULL),
    ADD CONSTRAINT kyc_documents_mime_type_check CHECK (mime_type IN ('image/jpeg', 'image/png', 'application/pdf')),
    ADD CONSTRAINT kyc_documents_file_size_check CHECK (file_size > 0),
    ADD CONSTRAINT kyc_documents_storage_path_check CHECK (storage_path ~ '^kyc/[0-9a-f-]{36}/[0-9a-f-]{36}$');
CREATE INDEX kyc_documents_application ON kyc_documents (application_id, created_at);

-- The new sealed tables may have findings; every table of before stays.
ALTER TABLE integrity_findings
    DROP CONSTRAINT integrity_findings_table_name_check,
    ADD CONSTRAINT integrity_findings_table_name_check CHECK (
        table_name IN ('users', 'user_contacts', 'sessions', 'user_passwords', 'user_passkeys',
            'user_totp', 'account_recoveries', 'session_step_ups',
            'admin_sessions', 'user_restrictions', 'platform_controls', 'user_recovery_codes',
            'balances', 'withdrawal_destinations', 'operation_confirmations', 'user_confirmation_methods',
            'user_anti_phishing_codes', 'sms_routing', 'currency_networks', 'markets', 'withdrawals', 'integrity_findings',
            'withdrawal_payouts', 'deny_links', 'price_overrides',
            'currency_migrations', 'currency_delistings',
            'otc_trades', 'otc_house_positions',
            'venue_reservations', 'venue_holds',
            'otc_routing_rules', 'otc_routing_usage',
            'otc_routed_orders',
            'user_national_ids', 'identity_checks',
            'aml_rules', 'aml_rule_history', 'aml_events', 'aml_alerts',
            'sanctions_lists',
            'kyc_applications', 'kyc_documents'));
