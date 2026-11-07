-- The refresh token that a session had before its last rotation, and when
-- it was rotated (#194). Two tabs of a browser share the refresh cookie and
-- may refresh at the same moment; the one that loses the rotation presents
-- the previous token. Within the grace period, it gets a new access token
-- instead of revoking the session as a reuse.
ALTER TABLE sessions
    ADD COLUMN previous_refresh_hash TEXT,
    ADD COLUMN rotated_at TIMESTAMPTZ;
