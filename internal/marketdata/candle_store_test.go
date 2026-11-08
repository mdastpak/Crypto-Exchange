package marketdata

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

func candle(at time.Time, price string) Candle {
	p := shared.MustNewDecimalFromString(price)
	return Candle{OpenTime: at, Open: p, High: p, Low: p, Close: p, Volume: shared.MustNewDecimalFromString("1.5")}
}

func TestWriteCandles_CreatesPartitionsAndWidensTheCursor(t *testing.T) {
	store, db := newStore(t)
	ctx := context.Background()
	pair := newPair(t, store)
	from := time.Date(2026, 10, 31, 22, 0, 0, 0, time.UTC)
	until := from.Add(4 * time.Hour)
	candles := []Candle{candle(from, "1"), candle(from.Add(time.Hour), "2"), candle(from.Add(3*time.Hour), "68000.123456789012345678")}

	n, err := store.WriteCandles(ctx, pair, Resolution1h, from, until, candles)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	parts, err := store.CandlePartitions(ctx, Resolution1h)
	require.NoError(t, err)
	assert.Equal(t, []string{"market_candles_1h_2026_10", "market_candles_1h_2026_11"}, parts, "a range across months")

	got, err := store.Candles(ctx, pair, Resolution1h, from, until)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "68000.123456789012345678", got[2].Close.String())

	// The same candles again change nothing; a backward range widens the
	// cursor.
	n, err = store.WriteCandles(ctx, pair, Resolution1h, from, until, candles)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	_, err = store.WriteCandles(ctx, pair, Resolution1h, from.Add(-2*time.Hour), from, nil)
	require.NoError(t, err, "an empty answer still moves the cursor")

	_, err = db.Exec(`UPDATE market_venues SET enabled = TRUE`)
	require.NoError(t, err)
	btc, _, err := store.CreateAsset(ctx, "bitcoin", "coingecko", "bitcoin", bitcoin())
	require.NoError(t, err)
	_, err = store.UpsertVenuePairs(ctx, "binance", []VenuePair{{VenueSymbol: "BTCUSDT", BaseSymbol: "BTC", QuoteSymbol: "USDT", BaseAssetID: &btc}})
	require.NoError(t, err)
	pairs, err := store.CandlePairs(ctx, "binance", Resolution1h, []string{"usdt"}, 10)
	require.NoError(t, err)
	require.Len(t, pairs, 1)
	assert.Equal(t, from.Add(-2*time.Hour), pairs[0].From.UTC())
	assert.Equal(t, until, pairs[0].Until.UTC())
	none, err := store.CandlePairs(ctx, "binance", Resolution1h, []string{"EUR"}, 10)
	require.NoError(t, err)
	assert.Empty(t, none, "only the configured quotes")

	_, err = store.WriteCandles(ctx, pair, Resolution1h, from, until, []Candle{candle(until, "1")})
	assert.Error(t, err, "a candle outside the range")
	_, err = store.WriteCandles(ctx, pair, Resolution1h, from, until, []Candle{candle(from.Add(time.Minute), "1")})
	assert.Error(t, err, "a candle not aligned")
	_, err = store.WriteCandles(ctx, pair, "2h", from, until, nil)
	assert.Error(t, err)
}

func TestCandlePartitions_EnsureAndRetention(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, 12, 15, 10, 0, 0, 0, time.UTC)

	require.NoError(t, store.EnsureCandlePartitions(ctx, Resolution1h, now))
	require.NoError(t, store.EnsureCandlePartitions(ctx, Resolution1h, now), "twice is harmless")
	require.NoError(t, store.EnsureCandlePartitions(ctx, Resolution1d, now))
	hourly, err := store.CandlePartitions(ctx, Resolution1h)
	require.NoError(t, err)
	assert.Equal(t, []string{"market_candles_1h_2026_12", "market_candles_1h_2027_01"}, hourly, "this month and the next")
	daily, err := store.CandlePartitions(ctx, Resolution1d)
	require.NoError(t, err)
	assert.Equal(t, []string{"market_candles_1d_2026", "market_candles_1d_2027"}, daily, "this year and the next")

	pair := newPair(t, store)
	_, err = store.WriteCandles(ctx, pair, Resolution1h, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC), []Candle{candle(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), "1")})
	require.NoError(t, err)

	// Retention drops whole partitions that end before the limit only.
	dropped, err := store.DropCandlePartitions(ctx, Resolution1h, time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, 1, dropped, "June; December ends after the limit")
	hourly, err = store.CandlePartitions(ctx, Resolution1h)
	require.NoError(t, err)
	assert.Equal(t, []string{"market_candles_1h_2026_12", "market_candles_1h_2027_01"}, hourly)
	dropped, err = store.DropCandlePartitions(ctx, Resolution1d, time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, 0, dropped, "2026 ends after the limit")
}
