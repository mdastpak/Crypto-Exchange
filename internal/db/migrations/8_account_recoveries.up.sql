-- Account recovery (#307): how a user who lost the account password and the
-- recovery codes gets the account back. A recovery proves every verified
-- contact of the account, in the strict mode also the identity of its
-- owner, and then waits, so that the owner can cancel it with the password.
CREATE TABLE account_recoveries (
    id UUID PRIMARY KEY,
    user_hk UUID NOT NULL REFERENCES users (hk),
    mode TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'verifying',
    -- The ids of the contacts that were proved with a code.
    proven_contacts BIGINT[] NOT NULL DEFAULT '{}',
    -- The identity steps that the recovery needs, and those that passed.
    steps_required TEXT[] NOT NULL DEFAULT '{}',
    steps_passed TEXT[] NOT NULL DEFAULT '{}',
    identity_failures INTEGER NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- When the wait ends, once everything is proved.
    ready_at TIMESTAMPTZ,
    -- When the recovery stops being usable.
    expires_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ,
    end_reason TEXT,
    ip INET,
    user_agent TEXT,
    CONSTRAINT account_recoveries_mode_check CHECK (mode IN ('standard', 'strict')),
    CONSTRAINT account_recoveries_status_check CHECK (status IN ('verifying', 'waiting', 'completed', 'cancelled', 'failed')),
    CONSTRAINT account_recoveries_ended_check CHECK ((status IN ('verifying', 'waiting')) = (ended_at IS NULL)),
    CONSTRAINT account_recoveries_ready_check CHECK (status <> 'waiting' OR ready_at IS NOT NULL),
    CONSTRAINT account_recoveries_failures_check CHECK (identity_failures >= 0)
);

-- A user has one open recovery at a time.
CREATE UNIQUE INDEX account_recoveries_open_key ON account_recoveries (user_hk) WHERE status IN ('verifying', 'waiting');
CREATE INDEX account_recoveries_user_idx ON account_recoveries (user_hk, started_at DESC);
