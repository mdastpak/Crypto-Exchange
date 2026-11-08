-- The global market data catalog (#262, #263): assets, exchanges (venues),
-- their pairs and latest tickers, and the runs of the jobs that collect them,
-- which cmd/market-worker writes from external sources such as CoinGecko and
-- the public APIs of exchanges.
--
-- These tables are separate from the currencies listed on this exchange: an
-- asset of the catalog is "unverified" and can never be traded through
-- them. An administrator may link it to a listed currency, which changes
-- nothing on the currency. Every table has the market_ prefix and no other
-- table refers to them, so the catalog can move to a database of its own.
--
-- Amounts are NUMERIC, never floating point. Nothing is deleted by the
-- runtime role: an asset or a pair that a source drops is marked inactive.

CREATE TABLE market_assets (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- slug names the asset in URLs, such as "bitcoin". Symbols collide
    -- between assets, so the symbol is not a key.
    slug               TEXT NOT NULL UNIQUE
                       CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND length(slug) <= 100),
    symbol             TEXT NOT NULL
                       CHECK (symbol = upper(symbol) AND length(symbol) BETWEEN 1 AND 32),
    name               TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    kind               TEXT NOT NULL DEFAULT 'unknown'
                       CHECK (kind IN ('coin', 'token', 'unknown')),
    -- unverified: collected, shown with an "unverified" label;
    -- hidden: never served; linked: the same asset as a listed currency.
    status             TEXT NOT NULL DEFAULT 'unverified'
                       CHECK (status IN ('unverified', 'hidden', 'linked')),
    currency_hk        UUID,
    -- active is false once no source lists the asset any more.
    active             BOOLEAN NOT NULL DEFAULT TRUE,
    -- description is plain text, sanitized by the job; source HTML is
    -- never stored.
    description        TEXT NOT NULL DEFAULT '' CHECK (length(description) <= 20000),
    -- links holds the https links of the asset by kind, such as website,
    -- explorer, and whitepaper.
    links              JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(links) = 'object'),
    logo_url           TEXT NOT NULL DEFAULT '' CHECK (logo_url = '' OR logo_url LIKE 'https://%'),
    market_cap_rank    INTEGER CHECK (market_cap_rank > 0),
    circulating_supply NUMERIC CHECK (circulating_supply >= 0),
    total_supply       NUMERIC CHECK (total_supply >= 0),
    max_supply         NUMERIC CHECK (max_supply >= 0),
    -- attribution names the source of the data, which its license asks
    -- to show with it.
    attribution        TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT market_assets_link_check
        CHECK ((status = 'linked') = (currency_hk IS NOT NULL))
);

CREATE INDEX market_assets_symbol ON market_assets (symbol);
CREATE INDEX market_assets_rank ON market_assets (market_cap_rank) WHERE market_cap_rank IS NOT NULL;
-- A listed currency is linked to at most one asset of the catalog.
CREATE UNIQUE INDEX market_assets_currency ON market_assets (currency_hk) WHERE currency_hk IS NOT NULL;

-- The identity of an asset at each source, such as its CoinGecko id.
CREATE TABLE market_asset_sources (
    source      TEXT NOT NULL CHECK (source ~ '^[a-z0-9_]{1,40}$'),
    external_id TEXT NOT NULL CHECK (length(external_id) BETWEEN 1 AND 200),
    asset_id    BIGINT NOT NULL REFERENCES market_assets (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (source, external_id)
);

CREATE INDEX market_asset_sources_asset ON market_asset_sources (asset_id);

-- The contracts of a token, one per network, which match the same token
-- across sources. The address is stored as the store normalizes it: lower
-- case for the hex addresses of EVM networks.
CREATE TABLE market_asset_contracts (
    network    TEXT NOT NULL CHECK (network ~ '^[a-z0-9_-]{1,60}$'),
    address    TEXT NOT NULL CHECK (length(address) BETWEEN 1 AND 200),
    asset_id   BIGINT NOT NULL REFERENCES market_assets (id),
    source     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (network, address)
);

CREATE INDEX market_asset_contracts_asset ON market_asset_contracts (asset_id);

-- The exchanges whose markets the catalog compares.
CREATE TABLE market_venues (
    id          TEXT PRIMARY KEY CHECK (id ~ '^[a-z0-9_]{1,40}$'),
    name        TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    -- cex: a centralized exchange; dex: a decentralized one.
    kind        TEXT NOT NULL CHECK (kind IN ('cex', 'dex')),
    -- region is the market the venue serves: global, or ir for the
    -- Iranian exchanges, which a release for users outside Iran leaves out
    -- (#272).
    region      TEXT NOT NULL CHECK (region IN ('global', 'ir')),
    country     TEXT NOT NULL DEFAULT '' CHECK (country = '' OR country ~ '^[A-Z]{2}$'),
    website     TEXT NOT NULL DEFAULT '' CHECK (website = '' OR website LIKE 'https://%'),
    attribution TEXT NOT NULL DEFAULT '',
    -- enabled says whether the jobs collect the venue.
    enabled     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- A pair at a venue. The base and quote assets are set once the pair is
-- mapped to the catalog; an unmapped pair waits for review.
CREATE TABLE market_venue_pairs (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    venue_id       TEXT NOT NULL REFERENCES market_venues (id),
    -- venue_symbol is the pair as the venue names it, such as BTCUSDT.
    venue_symbol   TEXT NOT NULL CHECK (length(venue_symbol) BETWEEN 1 AND 60),
    base_symbol    TEXT NOT NULL CHECK (base_symbol = upper(base_symbol) AND length(base_symbol) BETWEEN 1 AND 32),
    quote_symbol   TEXT NOT NULL CHECK (quote_symbol = upper(quote_symbol) AND length(quote_symbol) BETWEEN 1 AND 32),
    base_asset_id  BIGINT REFERENCES market_assets (id),
    quote_asset_id BIGINT REFERENCES market_assets (id),
    active         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (venue_id, venue_symbol)
);

CREATE INDEX market_venue_pairs_base ON market_venue_pairs (base_asset_id) WHERE base_asset_id IS NOT NULL;

-- The latest ticker of each pair. A write only replaces an older
-- observation, so that two nodes, or a slow job, never put an older ticker
-- back.
CREATE TABLE market_tickers_latest (
    pair_id      BIGINT PRIMARY KEY REFERENCES market_venue_pairs (id),
    last_price   NUMERIC NOT NULL CHECK (last_price >= 0),
    bid          NUMERIC CHECK (bid >= 0),
    ask          NUMERIC CHECK (ask >= 0),
    -- The figures of the last 24 hours, as the venue reports them.
    volume_base  NUMERIC CHECK (volume_base >= 0),
    volume_quote NUMERIC CHECK (volume_quote >= 0),
    high         NUMERIC CHECK (high >= 0),
    low          NUMERIC CHECK (low >= 0),
    open_price   NUMERIC CHECK (open_price >= 0),
    -- observed_at is the time of the ticker at the venue; fetched_at is
    -- when the job received it.
    observed_at  TIMESTAMPTZ NOT NULL,
    fetched_at   TIMESTAMPTZ NOT NULL
);

-- One row per run of a job, for operations and the admin view.
CREATE TABLE market_sync_runs (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job           TEXT NOT NULL CHECK (job ~ '^[a-z0-9_.:-]{1,100}$'),
    -- node is the application name of the worker that ran it.
    node          TEXT NOT NULL,
    mode          TEXT NOT NULL CHECK (mode IN ('live', 'dry_run')),
    status        TEXT NOT NULL DEFAULT 'running'
                  CHECK (status IN ('running', 'succeeded', 'failed')),
    started_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at   TIMESTAMPTZ,
    items_read    INTEGER NOT NULL DEFAULT 0 CHECK (items_read >= 0),
    items_written INTEGER NOT NULL DEFAULT 0 CHECK (items_written >= 0),
    error         TEXT NOT NULL DEFAULT '' CHECK (length(error) <= 4000),
    CONSTRAINT market_sync_runs_finished_check
        CHECK ((status = 'running') = (finished_at IS NULL))
);

CREATE INDEX market_sync_runs_job ON market_sync_runs (job, started_at DESC);
