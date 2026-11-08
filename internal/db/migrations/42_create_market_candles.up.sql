-- The candles of the pairs of the venues (#268): OHLCV history for the
-- price charts of each exchange. Partitioned by resolution, then by time:
-- a month per partition for 5m and 1h, a year for 1d. Retention drops whole
-- partitions, so that old candles go without a DELETE and without bloat.
CREATE TABLE market_candles (
    pair_id    BIGINT NOT NULL REFERENCES market_venue_pairs (id),
    resolution TEXT NOT NULL CHECK (resolution IN ('5m', '1h', '1d')),
    open_time  TIMESTAMPTZ NOT NULL,
    open       NUMERIC NOT NULL CHECK (open >= 0),
    high       NUMERIC NOT NULL CHECK (high >= 0),
    low        NUMERIC NOT NULL CHECK (low >= 0),
    close      NUMERIC NOT NULL CHECK (close >= 0),
    -- volume is in the base asset.
    volume     NUMERIC NOT NULL CHECK (volume >= 0),
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT market_candles_range_check CHECK (low <= high),
    PRIMARY KEY (pair_id, resolution, open_time)
) PARTITION BY LIST (resolution);

CREATE TABLE market_candles_5m PARTITION OF market_candles FOR VALUES IN ('5m') PARTITION BY RANGE (open_time);
CREATE TABLE market_candles_1h PARTITION OF market_candles FOR VALUES IN ('1h') PARTITION BY RANGE (open_time);
CREATE TABLE market_candles_1d PARTITION OF market_candles FOR VALUES IN ('1d') PARTITION BY RANGE (open_time);

-- What the candle jobs have asked each venue for, per pair and resolution:
-- the candles in [from_time, until_time) are stored, or the venue had none.
-- A run continues forward from until_time, and backfills backward from
-- from_time, so that a restart resumes where the last run stopped and an
-- empty answer is not asked again.
CREATE TABLE market_candle_cursors (
    pair_id    BIGINT NOT NULL REFERENCES market_venue_pairs (id),
    resolution TEXT NOT NULL CHECK (resolution IN ('5m', '1h', '1d')),
    from_time  TIMESTAMPTZ NOT NULL,
    until_time TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT market_candle_cursors_range_check CHECK (from_time <= until_time),
    PRIMARY KEY (pair_id, resolution)
);

-- The period of the partitions of a resolution.
CREATE FUNCTION market_candles_period(p_resolution TEXT) RETURNS TEXT
    LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE p_resolution WHEN '1d' THEN 'year' WHEN '1h' THEN 'month' WHEN '5m' THEN 'month' END
$$;

-- market_candles_ensure_partition creates the partition of a resolution
-- that holds p_at, if it does not exist, and returns its name. The runtime
-- role cannot create tables; this function, which runs as its owner, lets
-- it create only these partitions. Workers that race take a transaction
-- lock on the name, which works behind PgBouncer.
CREATE FUNCTION market_candles_ensure_partition(p_resolution TEXT, p_at TIMESTAMPTZ) RETURNS TEXT
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    period TEXT := market_candles_period(p_resolution);
    lo     TIMESTAMP;
    name   TEXT;
BEGIN
    IF period IS NULL OR p_at IS NULL THEN
        RAISE EXCEPTION 'market candles: no partition for % at %', p_resolution, p_at;
    END IF;
    lo := date_trunc(period, p_at AT TIME ZONE 'UTC');
    name := 'market_candles_' || p_resolution || '_' ||
        to_char(lo, CASE period WHEN 'year' THEN 'YYYY' ELSE 'YYYY_MM' END);
    PERFORM pg_advisory_xact_lock(hashtext(name));
    IF to_regclass(name) IS NULL THEN
        EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
            name, 'market_candles_' || p_resolution,
            (lo AT TIME ZONE 'UTC'), ((lo + ('1 ' || period)::interval) AT TIME ZONE 'UTC'));
    END IF;
    RETURN name;
END
$$;

-- market_candles_drop_partitions drops the partitions of a resolution that
-- end at or before p_before, for retention, and returns how many. It drops
-- only partitions that market_candles_ensure_partition names.
CREATE FUNCTION market_candles_drop_partitions(p_resolution TEXT, p_before TIMESTAMPTZ) RETURNS INTEGER
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
DECLARE
    period  TEXT := market_candles_period(p_resolution);
    part    RECORD;
    lo      TIMESTAMP;
    dropped INTEGER := 0;
BEGIN
    IF period IS NULL OR p_before IS NULL THEN
        RAISE EXCEPTION 'market candles: no partitions of % before %', p_resolution, p_before;
    END IF;
    FOR part IN
        SELECT c.relname FROM pg_inherits i
        JOIN pg_class c ON c.oid = i.inhrelid
        JOIN pg_class p ON p.oid = i.inhparent
        WHERE p.relname = 'market_candles_' || p_resolution
            AND c.relname ~ ('^market_candles_' || p_resolution || '_[0-9]{4}(_[0-9]{2})?$')
    LOOP
        IF period = 'year' THEN
            lo := to_timestamp(right(part.relname, 4), 'YYYY')::timestamp;
        ELSE
            lo := to_timestamp(right(part.relname, 7), 'YYYY_MM')::timestamp;
        END IF;
        IF ((lo + ('1 ' || period)::interval) AT TIME ZONE 'UTC') <= p_before THEN
            EXECUTE format('DROP TABLE %I', part.relname);
            dropped := dropped + 1;
        END IF;
    END LOOP;
    RETURN dropped;
END
$$;

-- Only the roles that the migrations grant may call them.
REVOKE ALL ON FUNCTION market_candles_ensure_partition(TEXT, TIMESTAMPTZ) FROM PUBLIC;
REVOKE ALL ON FUNCTION market_candles_drop_partitions(TEXT, TIMESTAMPTZ) FROM PUBLIC;
