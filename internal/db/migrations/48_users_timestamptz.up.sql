-- The timestamps of users were TIMESTAMP without a time zone (migration 2),
-- so a value depended on the session time zone of whoever wrote it, and
-- comparing deleted_at with a TIMESTAMPTZ such as security_hold_until
-- depended on it too (#315). As for currencies in migration 24, the values
-- written so far came from NOW() in UTC sessions and are read as UTC; so the
-- canonical text of deleted_at in its row seal, which was the stored wall
-- time, is the same instant formatted in UTC, and the seals stay valid.
-- A row without a creation time gets its update time, or now.
UPDATE users SET
    created_at = COALESCE(created_at, updated_at, NOW()),
    updated_at = COALESCE(updated_at, created_at, NOW())
WHERE created_at IS NULL OR updated_at IS NULL;

ALTER TABLE users
    ALTER COLUMN created_at TYPE TIMESTAMPTZ USING created_at AT TIME ZONE 'UTC',
    ALTER COLUMN updated_at TYPE TIMESTAMPTZ USING updated_at AT TIME ZONE 'UTC',
    ALTER COLUMN deleted_at TYPE TIMESTAMPTZ USING deleted_at AT TIME ZONE 'UTC',
    ALTER COLUMN created_at SET NOT NULL,
    ALTER COLUMN updated_at SET NOT NULL;
