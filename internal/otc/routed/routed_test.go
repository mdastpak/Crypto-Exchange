package routed

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// TestLimit covers the slippage guard of #387: the worst price of an order
// keeps the trade whole after the venue's fee, and stays within the
// slippage of the reference, on the venue's tick.
func TestLimit(t *testing.T) {
	for name, c := range map[string]struct {
		side, price, reference string
		fee, slip              int
		tick, want             string
	}{
		"buy by the slippage":    {"buy", "61000", "60000", 10, 50, "0.01", "60300"},
		"buy by the trade":       {"buy", "60100", "60000", 10, 50, "0.01", "60039.96"},
		"sell by the slippage":   {"sell", "59000", "60000", 10, 50, "0.01", "59700"},
		"sell by the trade":      {"sell", "59900", "60000", 10, 50, "0.01", "59959.96"},
		"buy floors to the tick": {"buy", "100", "1.23456", 0, 0, "0.001", "1.234"},
		"sell ceils to the tick": {"sell", "0.0001", "1.23456", 0, 0, "0.001", "1.235"},
		"no tick":                {"buy", "100", "1.23456", 0, 0, "0", "1.23456"},
		"a fee of the whole":     {"sell", "100", "100", 10000, 0, "0", "0"},
	} {
		t.Run(name, func(t *testing.T) {
			got := Limit(c.side, d(c.price), d(c.reference), c.fee, c.slip, d(c.tick))
			assert.True(t, got.Equal(d(c.want)), "want %s, got %s", c.want, got)
		})
	}
}

func TestClientID(t *testing.T) {
	id := ClientID("6F9619FF-8B86-D011-B42D-00C04FC964FF", 0, 1)
	assert.Len(t, id, 32)
	assert.Regexp(t, `^r[0-9a-f]{31}$`, id)
	assert.Equal(t, id, ClientID("6f9619ff-8b86-d011-b42d-00c04fc964ff", 0, 1), "of the trade, whatever its case")
	assert.NotEqual(t, id, ClientID("6f9619ff-8b86-d011-b42d-00c04fc964ff", 0, 2))
	assert.NotEqual(t, id, ClientID("6f9619ff-8b86-d011-b42d-00c04fc964ff", 1, 1))
}

func TestBeyond(t *testing.T) {
	assert.True(t, beyond("buy", d("101"), d("100")))
	assert.False(t, beyond("buy", d("100"), d("100")))
	assert.True(t, beyond("sell", d("99"), d("100")))
	assert.False(t, beyond("sell", d("100"), d("100")))
}

func TestSpends(t *testing.T) {
	o := Order{Side: "buy", BaseAsset: "BTC", QuoteAsset: "USDT", Quantity: d("0.5"), Limit: d("60000.01")}
	asset, amount := o.Spends(10)
	assert.Equal(t, "USDT", asset)
	assert.True(t, amount.Equal(d("30030.005005")), amount.String())
	o.Side = "sell"
	asset, amount = o.Spends(10)
	assert.Equal(t, "BTC", asset)
	assert.True(t, amount.Equal(d("0.5")))
}
