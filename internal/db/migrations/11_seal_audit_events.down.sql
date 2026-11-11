-- The seals are lost, so the events written since migration 11 can no longer
-- be checked.
ALTER TABLE audit_events
    DROP CONSTRAINT IF EXISTS audit_events_seal_complete,
    DROP COLUMN IF EXISTS seal_key_version,
    DROP COLUMN IF EXISTS row_seal,
    DROP COLUMN IF EXISTS row_version;
