-- The lifecycle of a currency (#216): the step of a delisting it is at, and
-- the capabilities that follow from it, which administrators can override.
-- A delisting is a schedule of steps; the scheduler of every instance
-- applies the steps that are due, each exactly once.
ALTER TABLE currencies
    ADD COLUMN lifecycle TEXT NOT NULL DEFAULT 'active'
        CHECK (lifecycle IN ('active', 'delist_announced', 'deposits_disabled', 'sell_only',
                             'trading_halted', 'withdrawal_only', 'frozen', 'delisted')),
    ADD COLUMN lifecycle_changed_at TIMESTAMPTZ;

-- An override sets one capability of a currency, whatever its lifecycle
-- says, until it is removed.
CREATE TABLE currency_capability_overrides (
    currency_hk UUID        NOT NULL REFERENCES currencies (hk),
    capability  TEXT        NOT NULL CHECK (capability IN ('deposit', 'withdraw', 'buy', 'sell',
                                                           'convert_into', 'convert_from', 'listed')),
    allowed     BOOLEAN     NOT NULL,
    note        TEXT        CHECK (length(note) <= 1000),
    created_by  UUID        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (currency_hk, capability)
);

CREATE TABLE currency_delistings (
    hk                     UUID        PRIMARY KEY,
    currency_hk            UUID        NOT NULL REFERENCES currencies (hk),
    kind                   TEXT        NOT NULL CHECK (kind IN ('standard', 'emergency')),
    status                 TEXT        NOT NULL DEFAULT 'scheduled'
        CHECK (status IN ('scheduled', 'completed', 'cancelled')),
    -- What happens to the balances that are left at the frozen step. The
    -- settlement itself needs the ledger (#60); the policy is recorded now,
    -- so that it is announced with the schedule.
    settlement             TEXT        NOT NULL CHECK (settlement IN ('convert', 'claim')),
    settlement_currency_hk UUID        REFERENCES currencies (hk),
    price_source           TEXT        CHECK (price_source IN ('last_trade', 'twap', 'fixed')),
    fixed_price            NUMERIC(38, 18) CHECK (fixed_price > 0),
    claim_days             INTEGER     CHECK (claim_days BETWEEN 1 AND 3650),
    announcement_url       TEXT        CHECK (length(announcement_url) <= 2048),
    note                   TEXT        CHECK (length(note) <= 1000),
    created_by             UUID        NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    cancelled_at           TIMESTAMPTZ,
    cancelled_by           UUID,
    completed_at           TIMESTAMPTZ,
    version                BIGINT      NOT NULL DEFAULT 1,
    CHECK ((settlement = 'convert') = (settlement_currency_hk IS NOT NULL AND price_source IS NOT NULL)),
    CHECK ((settlement = 'claim') = (claim_days IS NOT NULL)),
    CHECK ((price_source = 'fixed') = (fixed_price IS NOT NULL)),
    CHECK (settlement_currency_hk IS DISTINCT FROM currency_hk),
    CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL AND cancelled_by IS NOT NULL)),
    CHECK ((status = 'completed') = (completed_at IS NOT NULL))
);

-- At most one delisting of a currency is under way.
CREATE UNIQUE INDEX idx_currency_delistings_one_open ON currency_delistings (currency_hk) WHERE status = 'scheduled';
CREATE INDEX idx_currency_delistings_currency ON currency_delistings (currency_hk, created_at DESC);

CREATE TABLE currency_delisting_steps (
    delisting_hk UUID        NOT NULL REFERENCES currency_delistings (hk) ON DELETE CASCADE,
    state        TEXT        NOT NULL CHECK (state IN ('delist_announced', 'deposits_disabled', 'sell_only',
                                                       'trading_halted', 'withdrawal_only', 'frozen', 'delisted')),
    scheduled_at TIMESTAMPTZ NOT NULL,
    applied_at   TIMESTAMPTZ,
    PRIMARY KEY (delisting_hk, state)
);

-- The scheduler looks for the steps that are due.
CREATE INDEX idx_currency_delisting_steps_due ON currency_delisting_steps (scheduled_at) WHERE applied_at IS NULL;
