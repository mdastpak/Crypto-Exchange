-- The requests made to each market data source (#265), counted across every
-- worker, so that the workers together keep within the limits of the
-- source's plan: per minute, and per month for plans with a monthly credit.
-- One row per source and window kind; a request in a new window resets the
-- count, so the table never grows.
CREATE TABLE market_source_usage (
    source       TEXT NOT NULL CHECK (source ~ '^[a-z0-9_]{1,40}$'),
    window_kind  TEXT NOT NULL CHECK (window_kind IN ('minute', 'month')),
    window_start TIMESTAMPTZ NOT NULL,
    requests     INTEGER NOT NULL CHECK (requests >= 0),
    PRIMARY KEY (source, window_kind)
);
