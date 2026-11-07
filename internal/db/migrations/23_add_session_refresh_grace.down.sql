ALTER TABLE sessions
    DROP COLUMN IF EXISTS rotated_at,
    DROP COLUMN IF EXISTS previous_refresh_hash;
