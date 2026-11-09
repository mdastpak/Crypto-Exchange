-- The administration of the market data catalog (#270). An administrator
-- pauses a source, such as coingecko or binance, when it must be left alone
-- (a changed API, a complaint, a plan running out): every worker stops
-- claiming the jobs that call it, and a run in progress stops at its next
-- batch of writes. The configuration still decides which sources exist; a
-- row here only pauses one. Rows are updated, never deleted.
CREATE TABLE market_source_controls (
    source     TEXT PRIMARY KEY CHECK (source ~ '^[a-z0-9_]{1,40}$'),
    paused     BOOLEAN NOT NULL DEFAULT FALSE,
    -- reason says why, for the administrators.
    reason     TEXT NOT NULL DEFAULT '' CHECK (length(reason) <= 1000),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The run history across every job, newest first, for the admin view.
CREATE INDEX market_sync_runs_started ON market_sync_runs (started_at DESC, id DESC);

-- The open conflicts, oldest first, for the admin view.
CREATE INDEX market_identity_conflicts_unresolved ON market_identity_conflicts (id) WHERE resolved_at IS NULL;
