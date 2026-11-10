-- The sessions of administrators (#308): every admin access token belongs to
-- one, so that it can be listed and ended in the database. A session is never
-- deleted; an ended one keeps when and why it ended.
CREATE TABLE admin_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    admin_id UUID NOT NULL REFERENCES users(hk) ON DELETE CASCADE,
    ip INET,
    user_agent TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    revoked_reason TEXT,
    CONSTRAINT admin_sessions_revoked_check CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL)),
    CONSTRAINT admin_sessions_revoked_reason_check CHECK (
        revoked_reason IN ('logout', 'revoked', 'replaced_at_sign_in', 'administrator_disabled', 'authenticator_reset', 'operator')),
    CONSTRAINT admin_sessions_user_agent_check CHECK (length(user_agent) <= 500)
);

CREATE INDEX admin_sessions_active ON admin_sessions (admin_id, created_at DESC, id DESC) WHERE revoked_at IS NULL;
