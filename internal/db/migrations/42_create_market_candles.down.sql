DROP FUNCTION IF EXISTS market_candles_drop_partitions(TEXT, TIMESTAMPTZ);
DROP FUNCTION IF EXISTS market_candles_ensure_partition(TEXT, TIMESTAMPTZ);
DROP FUNCTION IF EXISTS market_candles_period(TEXT);
DROP TABLE IF EXISTS market_candle_cursors;
DROP TABLE IF EXISTS market_candles;
