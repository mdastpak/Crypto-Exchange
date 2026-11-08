-- Restores the functions of migration 42, and drops the 1m and 15m candles.
CREATE OR REPLACE FUNCTION market_candles_period(p_resolution TEXT) RETURNS TEXT
    LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE p_resolution WHEN '1d' THEN 'year' WHEN '1h' THEN 'month' WHEN '5m' THEN 'month' END
$$;

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

DROP TABLE IF EXISTS market_candles_15m;
DROP TABLE IF EXISTS market_candles_1m;
DELETE FROM market_candle_cursors WHERE resolution IN ('1m', '15m');
ALTER TABLE market_candle_cursors DROP CONSTRAINT market_candle_cursors_resolution_check;
ALTER TABLE market_candle_cursors ADD CONSTRAINT market_candle_cursors_resolution_check
    CHECK (resolution IN ('5m', '1h', '1d'));
ALTER TABLE market_candles DROP CONSTRAINT market_candles_resolution_check;
ALTER TABLE market_candles ADD CONSTRAINT market_candles_resolution_check
    CHECK (resolution IN ('5m', '1h', '1d'));

ALTER TABLE market_assets DROP COLUMN IF EXISTS tier_override;
ALTER TABLE market_venue_pairs
    DROP COLUMN IF EXISTS tier_changed_at,
    DROP COLUMN IF EXISTS tier_override,
    DROP COLUMN IF EXISTS tier;
