-- The reason is restored without validating the sessions that ended for wrong
-- step-up codes, which stay as they are.
ALTER TABLE admin_sessions
    DROP CONSTRAINT admin_sessions_revoked_reason_check,
    ADD CONSTRAINT admin_sessions_revoked_reason_check CHECK (
        revoked_reason IN ('logout', 'revoked', 'replaced_at_sign_in', 'administrator_disabled', 'authenticator_reset', 'operator')) NOT VALID,
    DROP CONSTRAINT admin_sessions_step_up_failures_check,
    DROP COLUMN step_up_failures,
    DROP COLUMN elevated_until;
