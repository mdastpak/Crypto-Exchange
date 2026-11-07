-- The timestamps of currencies were TIMESTAMP without a time zone (migration
-- 1), so a value depended on the session time zone of whoever wrote it
-- (#196). The values written so far came from NOW() in UTC sessions, and are
-- read as UTC.
ALTER TABLE currencies
    ALTER COLUMN created_at TYPE TIMESTAMPTZ USING created_at AT TIME ZONE 'UTC',
    ALTER COLUMN updated_at TYPE TIMESTAMPTZ USING updated_at AT TIME ZONE 'UTC',
    ALTER COLUMN deleted_at TYPE TIMESTAMPTZ USING deleted_at AT TIME ZONE 'UTC';
