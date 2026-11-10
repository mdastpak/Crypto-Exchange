-- Step-up of administrators (#308): a session may use the sensitive admin
-- routes only until elevated_until, which a sign-in and a fresh authenticator
-- code set. Wrong codes are counted, and too many end the session.
ALTER TABLE admin_sessions
    ADD COLUMN elevated_until TIMESTAMPTZ,
    ADD COLUMN step_up_failures INTEGER NOT NULL DEFAULT 0,
    ADD CONSTRAINT admin_sessions_step_up_failures_check CHECK (step_up_failures >= 0),
    DROP CONSTRAINT admin_sessions_revoked_reason_check,
    ADD CONSTRAINT admin_sessions_revoked_reason_check CHECK (
        revoked_reason IN ('logout', 'revoked', 'replaced_at_sign_in', 'administrator_disabled', 'authenticator_reset', 'operator', 'step_up_failed'));
