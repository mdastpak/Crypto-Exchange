ALTER TABLE integrity_findings
    DROP CONSTRAINT IF EXISTS integrity_findings_reseal_reviewed_check,
    DROP COLUMN IF EXISTS reseal_row_values,
    DROP COLUMN IF EXISTS reseal_digest_key_version,
    DROP COLUMN IF EXISTS reseal_row_digest,
    DROP COLUMN IF EXISTS reseal_row_version;
