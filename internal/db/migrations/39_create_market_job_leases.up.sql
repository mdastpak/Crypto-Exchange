-- The leases of the market data jobs (#264). Every worker of
-- cmd/market-worker runs the same jobs, and a lease makes each run happen
-- on one worker only. A worker claims a due job by raising its token and
-- setting lease_until, which it renews while the run goes on; a worker that
-- dies stops renewing, and another one claims the job once lease_until has
-- passed. The token fences a worker that lost its lease: it checks the
-- token before each batch of writes and stops when it changed. Every time is
-- the database's, so that the clocks of the workers do not matter.
CREATE TABLE market_job_leases (
    job              TEXT PRIMARY KEY CHECK (job ~ '^[a-z0-9_.:-]{1,100}$'),
    holder           TEXT NOT NULL DEFAULT '',
    token            BIGINT NOT NULL DEFAULT 0,
    lease_until      TIMESTAMPTZ NOT NULL DEFAULT '-infinity',
    -- next_run_at is when the job is due next.
    next_run_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- failures counts the failed runs in a row, for the backoff.
    failures         INTEGER NOT NULL DEFAULT 0 CHECK (failures >= 0),
    last_started_at  TIMESTAMPTZ,
    last_finished_at TIMESTAMPTZ,
    last_success_at  TIMESTAMPTZ
);

-- A lockdown of market_data pauses every job on every worker, for
-- maintenance or when a source must be left alone at once. Users never see
-- it. Servers of the previous release read such a row without trouble: an
-- unknown feature matches none of theirs.
ALTER TABLE platform_controls DROP CONSTRAINT platform_controls_feature_check;
ALTER TABLE platform_controls ADD CONSTRAINT platform_controls_feature_check
    CHECK (feature IN ('sign_up', 'sign_in', 'profile_changes', 'deposits', 'withdrawals', 'trading',
                       'api_keys', 'websocket', 'read_only', 'scheduler', 'outbox', 'market_data'));
