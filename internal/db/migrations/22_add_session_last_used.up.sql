-- When a session was last used (#193), for the session list of the user.
-- Every request with an access token of the session sets it, at most once a
-- minute; a refresh sets it too. NULL means not since the migration, and
-- the list shows the creation time instead.
ALTER TABLE sessions ADD COLUMN last_used_at TIMESTAMPTZ;

-- The session list reads the active sessions of a user, newest first.
CREATE INDEX idx_sessions_user_active ON sessions (user_id, created_at DESC, id DESC)
    WHERE status = 'ACTIVE';
