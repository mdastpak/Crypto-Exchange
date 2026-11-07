-- Migrations of currencies (#215): a rebrand, a redenomination, or a merger
-- that moves the holders of one or more source currencies to one target,
-- each source at its own ratio. A migration goes through scheduled phases;
-- the scheduler of every instance applies the phases that are due, each
-- exactly once. The conversion of balances itself needs the ledger (#60).
CREATE TABLE currency_migrations (
    hk               UUID        PRIMARY KEY,
    target_hk        UUID        NOT NULL REFERENCES currencies (hk),
    -- A forced migration pauses and retires its sources; an optional one,
    -- such as MKR to SKY, only offers the conversion.
    forced           BOOLEAN     NOT NULL,
    phase            TEXT        NOT NULL DEFAULT 'scheduled'
        CHECK (phase IN ('scheduled', 'announced', 'deposits_paused', 'trading_halted', 'converting', 'completed', 'cancelled')),
    announcement_url TEXT        CHECK (length(announcement_url) <= 2048),
    note             TEXT        CHECK (length(note) <= 1000),
    created_by       UUID        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    cancelled_at     TIMESTAMPTZ,
    cancelled_by     UUID,
    completed_at     TIMESTAMPTZ,
    version          BIGINT      NOT NULL DEFAULT 1,
    CHECK ((phase = 'cancelled') = (cancelled_at IS NOT NULL AND cancelled_by IS NOT NULL)),
    CHECK ((phase = 'completed') = (completed_at IS NOT NULL))
);

CREATE INDEX idx_currency_migrations_target ON currency_migrations (target_hk, created_at DESC);

-- The ratio is the units of the target that one unit of the source becomes:
-- 0.01 for 100 LEND = 1 AAVE, 24000 for 1 MKR = 24,000 SKY. Conversions
-- round down to the precision of the target and record the dust.
CREATE TABLE currency_migration_sources (
    migration_hk UUID            NOT NULL REFERENCES currency_migrations (hk) ON DELETE CASCADE,
    currency_hk  UUID            NOT NULL REFERENCES currencies (hk),
    ratio        NUMERIC(38, 18) NOT NULL CHECK (ratio > 0),
    PRIMARY KEY (migration_hk, currency_hk)
);

CREATE INDEX idx_currency_migration_sources_currency ON currency_migration_sources (currency_hk);

CREATE TABLE currency_migration_phases (
    migration_hk UUID        NOT NULL REFERENCES currency_migrations (hk) ON DELETE CASCADE,
    phase        TEXT        NOT NULL CHECK (phase IN ('announced', 'deposits_paused', 'trading_halted', 'converting', 'completed')),
    scheduled_at TIMESTAMPTZ NOT NULL,
    applied_at   TIMESTAMPTZ,
    PRIMARY KEY (migration_hk, phase)
);

CREATE INDEX idx_currency_migration_phases_due ON currency_migration_phases (scheduled_at) WHERE applied_at IS NULL;
