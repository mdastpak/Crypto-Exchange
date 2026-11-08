-- Collection tiers of the pairs (#279): hot, active, and other. The tier
-- of a pair decides at which candle resolutions it is collected. The
-- tiers job recomputes tier daily from the 24-hour volume; an
-- administrator's tier on the pair, or on its base asset, overrides it.
ALTER TABLE market_venue_pairs
    ADD COLUMN tier TEXT NOT NULL DEFAULT 'other' CHECK (tier IN ('hot', 'active', 'other')),
    ADD COLUMN tier_override TEXT CHECK (tier_override IN ('hot', 'active', 'other')),
    ADD COLUMN tier_changed_at TIMESTAMPTZ;

ALTER TABLE market_assets
    ADD COLUMN tier_override TEXT CHECK (tier_override IN ('hot', 'active', 'other'));

-- The resolutions 1m and 15m. 1m candles are partitioned by day, so that
-- their short retention drops them a day at a time.
ALTER TABLE market_candles DROP CONSTRAINT market_candles_resolution_check;
ALTER TABLE market_candles ADD CONSTRAINT market_candles_resolution_check
    CHECK (resolution IN ('1m', '5m', '15m', '1h', '1d'));
ALTER TABLE market_candle_cursors DROP CONSTRAINT market_candle_cursors_resolution_check;
ALTER TABLE market_candle_cursors ADD CONSTRAINT market_candle_cursors_resolution_check
    CHECK (resolution IN ('1m', '5m', '15m', '1h', '1d'));

CREATE TABLE market_candles_1m PARTITION OF market_candles FOR VALUES IN ('1m') PARTITION BY RANGE (open_time);
CREATE TABLE market_candles_15m PARTITION OF market_candles FOR VALUES IN ('15m') PARTITION BY RANGE (open_time);

CREATE OR REPLACE FUNCTION market_candles_period(p_resolution TEXT) RETURNS TEXT
    LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE p_resolution
        WHEN '1d' THEN 'year' WHEN '1h' THEN 'month' WHEN '15m' THEN 'month'
        WHEN '5m' THEN 'month' WHEN '1m' THEN 'day' END
$$;

-- As in migration 42, with partitions of a day: market_candles_1m_2026_11_08.
CREATE OR REPLACE FUNCTION market_candles_ensure_partition(p_resolution TEXT, p_at TIMESTAMPTZ) RETURNS TEXT
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
        to_char(lo, CASE period WHEN 'year' THEN 'YYYY' WHEN 'month' THEN 'YYYY_MM' ELSE 'YYYY_MM_DD' END);
    PERFORM pg_advisory_xact_lock(hashtext(name));
    IF to_regclass(name) IS NULL THEN
        EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
            name, 'market_candles_' || p_resolution,
            (lo AT TIME ZONE 'UTC'), ((lo + ('1 ' || period)::interval) AT TIME ZONE 'UTC'));
    END IF;
    RETURN name;
END
$$;

CREATE OR REPLACE FUNCTION market_candles_drop_partitions(p_resolution TEXT, p_before TIMESTAMPTZ) RETURNS INTEGER
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
            AND c.relname ~ ('^market_candles_' || p_resolution || CASE period
                WHEN 'year' THEN '_[0-9]{4}$' WHEN 'month' THEN '_[0-9]{4}_[0-9]{2}$' ELSE '_[0-9]{4}_[0-9]{2}_[0-9]{2}$' END)
    LOOP
        IF period = 'year' THEN
            lo := to_timestamp(right(part.relname, 4), 'YYYY')::timestamp;
        ELSIF period = 'month' THEN
            lo := to_timestamp(right(part.relname, 7), 'YYYY_MM')::timestamp;
        ELSE
            lo := to_timestamp(right(part.relname, 10), 'YYYY_MM_DD')::timestamp;
        END IF;
        IF ((lo + ('1 ' || period)::interval) AT TIME ZONE 'UTC') <= p_before THEN
            EXECUTE format('DROP TABLE %I', part.relname);
            dropped := dropped + 1;
        END IF;
    END LOOP;
    RETURN dropped;
END
$$;
