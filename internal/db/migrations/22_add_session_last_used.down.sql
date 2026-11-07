DROP INDEX IF EXISTS idx_sessions_user_active;
ALTER TABLE sessions DROP COLUMN IF EXISTS last_used_at;
