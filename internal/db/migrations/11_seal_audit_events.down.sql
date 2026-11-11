-- The seals are lost, so the events written since migration 11 can no longer
-- be checked, and the audit-mac keys, which sealed nothing else, go with
-- them.
DELETE FROM data_keys WHERE purpose = 'audit-mac';
ALTER TABLE data_keys
    DROP CONSTRAINT data_keys_purpose_check,
    ADD CONSTRAINT data_keys_purpose_check CHECK (purpose IN ('row-mac', 'field-enc', 'blind-index', 'otp-mac'));

ALTER TABLE audit_events
    DROP CONSTRAINT IF EXISTS audit_events_seal_complete,
    DROP COLUMN IF EXISTS seal_key_version,
    DROP COLUMN IF EXISTS row_seal,
    DROP COLUMN IF EXISTS row_version;
