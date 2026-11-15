package socket

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/price"
)

func priceUpdate(t *testing.T, prices ...price.Price) []byte {
	t.Helper()
	body, err := json.Marshal(price.Update{Event: price.EventPricesUpdate, Prices: prices, At: time.Now()})
	require.NoError(t, err)
	return body
}

func livePrice(market, mid string, updated time.Time) price.Price {
	d := shared.MustNewDecimalFromString(mid)
	return price.Price{Market: market, Status: price.StatusLive, Bid: d, Ask: d, Mid: d,
		Sources: []string{"binance"}, Updated: updated}
}

func TestPriceThrottle_SendsLiveChangedPricesOnly(t *testing.T) {
	var th priceThrottle
	now := time.Now()
	stale := livePrice("ETH/USDT", "3000", now)
	stale.Status = price.StatusStale
	forged := livePrice("<script>", "1", now)
	zero := livePrice("SOL/USDT", "0", now)

	data, latencies, err := th.sanitize(priceUpdate(t, livePrice("BTC/USDT", "64000", now.Add(-40*time.Millisecond)), stale, forged, zero), now)
	require.NoError(t, err)
	var event PricesEvent
	require.NoError(t, json.Unmarshal(data, &event))
	assert.Equal(t, price.EventPricesUpdate, event.Event)
	require.Len(t, event.Prices, 1)
	assert.Equal(t, "BTC/USDT", event.Prices[0].Market)
	assert.NotContains(t, string(data), "sources", "only the fields of the event")
	require.Len(t, latencies, 1)
	assert.Equal(t, 40*time.Millisecond, latencies[0])

	data, _, err = th.sanitize(priceUpdate(t, livePrice("BTC/USDT", "64001", now)), now.Add(50*time.Millisecond))
	require.NoError(t, err)
	assert.Nil(t, data, "within priceMinInterval")
	data, _, err = th.sanitize(priceUpdate(t, livePrice("BTC/USDT", "64000", now)), now.Add(time.Second))
	require.NoError(t, err)
	assert.Nil(t, data, "unchanged")
	data, _, err = th.sanitize(priceUpdate(t, livePrice("BTC/USDT", "64001", now)), now.Add(time.Second))
	require.NoError(t, err)
	assert.NotNil(t, data, "changed")
	data, _, err = th.sanitize(priceUpdate(t, livePrice("BTC/USDT", "64001", now)), now.Add(7*time.Second))
	require.NoError(t, err)
	assert.NotNil(t, data, "the heartbeat of an unchanged price")
}

func TestPriceThrottle_DropsWhatIsNotAnUpdate(t *testing.T) {
	var th priceThrottle
	for name, body := range map[string][]byte{
		"not JSON":       []byte("{"),
		"another event":  []byte(`{"event": "currency.update"}`),
		"too large":      make([]byte, maxPriceUpdateBytes+1),
		"too many items": priceUpdate(t, make([]price.Price, maxPricesPerUpdate+1)...),
	} {
		_, _, err := th.sanitize(body, time.Now())
		assert.Error(t, err, name)
	}
}
