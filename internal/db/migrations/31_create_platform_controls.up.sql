-- Platform controls (#235): a full maintenance of the service, or a lockdown
-- of one of its features, for everyone at once. A control applies from
-- starts_at until it is lifted or reaches ends_at, so a scheduled control
-- starts and ends without a job, on every instance at the same moment. Rows
-- are never deleted, so that the history stays.
CREATE TABLE platform_controls (
    hk                UUID        PRIMARY KEY,
    kind              TEXT        NOT NULL CHECK (kind IN ('maintenance', 'lockdown')),
    feature           TEXT        CHECK (feature IN ('sign_up', 'sign_in', 'profile_changes', 'deposits',
                                      'withdrawals', 'trading', 'api_keys', 'websocket', 'read_only',
                                      'scheduler', 'outbox')),
    starts_at         TIMESTAMPTZ NOT NULL,
    ends_at           TIMESTAMPTZ,
    -- announce_at is when users learn of a scheduled control, through the
    -- status endpoint and the WebSocket.
    announce_at       TIMESTAMPTZ NOT NULL,
    message_fa        TEXT        CHECK (length(message_fa) <= 500),
    message_en        TEXT        CHECK (length(message_en) <= 500),
    reason            TEXT        NOT NULL CHECK (length(reason) BETWEEN 1 AND 1000),
    -- Who may still use the service during a full maintenance: networks,
    -- such as the office or a VPN, and users, such as testers.
    allow_networks    CIDR[]      NOT NULL DEFAULT '{}',
    allow_users       UUID[]      NOT NULL DEFAULT '{}',
    -- created_by, lift_requested_by, and lifted_by are administrators. They
    -- are NULL for a change made with cmd/admin, which the audit log names.
    created_by        UUID,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    lift_requested_by UUID,
    lift_requested_at TIMESTAMPTZ,
    lifted_at         TIMESTAMPTZ,
    lifted_by         UUID,
    lift_note         TEXT        CHECK (length(lift_note) <= 1000),
    version           BIGINT      NOT NULL DEFAULT 1,
    CHECK ((kind = 'maintenance') = (feature IS NULL)),
    CHECK (kind = 'maintenance' OR (cardinality(allow_networks) = 0 AND cardinality(allow_users) = 0)),
    CHECK (ends_at IS NULL OR ends_at > starts_at),
    CHECK (announce_at <= starts_at),
    CHECK ((lift_requested_by IS NULL) = (lift_requested_at IS NULL)),
    CHECK (lifted_by IS NULL OR lifted_at IS NOT NULL)
);

-- Every instance reads the controls that are not lifted and have not ended.
CREATE INDEX idx_platform_controls_open ON platform_controls (starts_at) WHERE lifted_at IS NULL;
CREATE INDEX idx_platform_controls_created ON platform_controls (created_at DESC, hk DESC);

-- platform_state is one row whose version changes with every change of a
-- control, so that an instance learns of a change by reading one number.
-- The check of an operation takes a share lock of the row, and a change
-- takes the exclusive lock: a change waits for the operations in flight,
-- and the operations after it see it.
CREATE TABLE platform_state (
    id         BOOLEAN     PRIMARY KEY DEFAULT TRUE CHECK (id),
    version    BIGINT      NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO platform_state DEFAULT VALUES;
