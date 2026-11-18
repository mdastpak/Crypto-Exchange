DELETE FROM integrity_findings WHERE table_name IN ('kyc_applications', 'kyc_documents');
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
            'sanctions_lists'));

-- The applications and documents of the engine have no meaning in the
-- schema of before; the objects stay in the storage.
DELETE FROM kyc_documents;
DELETE FROM kyc_applications;
DROP INDEX IF EXISTS kyc_documents_application;
ALTER TABLE kyc_documents
    DROP CONSTRAINT kyc_documents_storage_path_check,
    DROP CONSTRAINT kyc_documents_file_size_check,
    DROP CONSTRAINT kyc_documents_mime_type_check,
    DROP CONSTRAINT kyc_documents_file_name_check,
    DROP COLUMN seal_key_version,
    DROP COLUMN row_seal,
    DROP COLUMN row_version,
    DROP COLUMN content_digest,
    DROP COLUMN application_id,
    ALTER COLUMN extracted_data_enc TYPE TEXT USING NULL,
    ALTER COLUMN file_name SET NOT NULL,
    ALTER COLUMN kyc_profile_id SET NOT NULL;

DROP INDEX IF EXISTS kyc_applications_queue;
DROP INDEX IF EXISTS kyc_applications_open;
ALTER TABLE kyc_applications ALTER COLUMN provider DROP NOT NULL;
ALTER TABLE kyc_applications ALTER COLUMN provider DROP DEFAULT;
ALTER TABLE kyc_applications
    DROP CONSTRAINT kyc_applications_review_notes_check,
    DROP CONSTRAINT kyc_applications_rejected_check,
    DROP CONSTRAINT kyc_applications_reviewed_check,
    DROP CONSTRAINT kyc_applications_submitted_check,
    DROP CONSTRAINT kyc_applications_provider_check,
    DROP CONSTRAINT kyc_applications_requested_tier_check,
    DROP COLUMN seal_key_version,
    DROP COLUMN row_seal,
    DROP COLUMN row_version,
    DROP COLUMN reason,
    DROP COLUMN submitted_at,
    ADD COLUMN documents UUID[] DEFAULT '{}'::UUID[],
    ALTER COLUMN form_data_enc TYPE TEXT USING NULL;

DELETE FROM kyc_profiles;
ALTER TABLE kyc_profiles
    ALTER COLUMN first_name_enc TYPE TEXT USING NULL,
    ALTER COLUMN last_name_enc TYPE TEXT USING NULL,
    ALTER COLUMN date_of_birth_enc TYPE DATE USING NULL,
    ALTER COLUMN nationality_enc TYPE VARCHAR(100) USING NULL,
    ALTER COLUMN country_of_residence_enc TYPE VARCHAR(100) USING NULL,
    ALTER COLUMN address_line1_enc TYPE TEXT USING NULL,
    ALTER COLUMN address_line2_enc TYPE TEXT USING NULL,
    ALTER COLUMN city_enc TYPE VARCHAR(100) USING NULL,
    ALTER COLUMN state_enc TYPE VARCHAR(100) USING NULL,
    ALTER COLUMN postal_code_enc TYPE VARCHAR(20) USING NULL,
    ALTER COLUMN country_enc TYPE VARCHAR(100) USING NULL;
