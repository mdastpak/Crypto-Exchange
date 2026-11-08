package exchange

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// Every venue source is also a candle source (#268). Each returns the
// closed candles of a pair that open in [from, to), oldest first, at most
// MaxCandles of them; the job asks for ranges of that size.

// closedIn keeps the candles that open in [from, to), are closed at now,
// and are aligned to the interval, oldest first, without duplicates.
func closedIn(candles []source.Candle, interval source.Interval, from, to, now time.Time) []source.Candle {
	step := interval.Duration()
	out := make([]source.Candle, 0, len(candles))
	seen := map[time.Time]bool{}
	for _, c := range candles {
		t := c.OpenTime.UTC()
		if t.Before(from) || !t.Before(to) || t.Add(step).After(now) || !t.Equal(t.Truncate(step)) || seen[t] {
			continue
		}
		if c.Low.Decimal().GreaterThan(c.High.Decimal()) {
			continue
		}
		seen[t] = true
		c.OpenTime = t
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b source.Candle) int { return a.OpenTime.Compare(b.OpenTime) })
	return out
}

// ohlcv reads a candle from its figures, and false when one is missing.
func ohlcv(at time.Time, open, high, low, closing, volume any) (source.Candle, bool) {
	o, h, l, c, v := nonNegative(open), nonNegative(high), nonNegative(low), nonNegative(closing), nonNegative(volume)
	if o == nil || h == nil || l == nil || c == nil || v == nil {
		return source.Candle{}, false
	}
	return source.Candle{OpenTime: at, Open: *o, High: *h, Low: *l, Close: *c, Volume: *v}, true
}

func at(values []any, i int) any {
	if i < len(values) {
		return values[i]
	}
	return nil
}

func unsupported(venue string, interval source.Interval) error {
	return fmt.Errorf("%s: no %s candles: %w", venue, interval, source.ErrNotFound)
}

// Binance: /api/v3/klines, 1000 per request.

func (b *binance) MaxCandles() int { return 1000 }

func (b *binance) Supports(source.Interval) bool { return true }

func (b *binance) Candles(ctx context.Context, venueSymbol string, interval source.Interval, from, to time.Time) ([]source.Candle, error) {
	if interval.Duration() == 0 {
		return nil, unsupported(Binance, interval)
	}
	q := url.Values{"symbol": {venueSymbol}, "interval": {string(interval)},
		"startTime": {strconv.FormatInt(from.UnixMilli(), 10)}, "endTime": {strconv.FormatInt(to.UnixMilli()-1, 10)},
		"limit": {strconv.Itoa(b.MaxCandles())}}
	var rows [][]any
	if err := b.client.GetJSON(ctx, "api/v3/klines", q, &rows); err != nil {
		return nil, err
	}
	out := make([]source.Candle, 0, len(rows))
	for _, r := range rows {
		t, err := source.UnixMillis(at(r, 0))
		if err != nil {
			continue
		}
		if c, ok := ohlcv(t, at(r, 1), at(r, 2), at(r, 3), at(r, 4), at(r, 5)); ok {
			out = append(out, c)
		}
	}
	return closedIn(out, interval, from, to, b.now()), nil
}

// OKX: /api/v5/market/history-candles, 100 per request, newest first; the
// daily bar of UTC days is 1Dutc.

var okxBars = map[source.Interval]string{source.Interval1m: "1m", source.Interval5m: "5m", source.Interval15m: "15m",
	source.Interval1h: "1H", source.Interval1d: "1Dutc"}

func (o *okx) MaxCandles() int { return 100 }

func (o *okx) Supports(interval source.Interval) bool { _, ok := okxBars[interval]; return ok }

func (o *okx) Candles(ctx context.Context, venueSymbol string, interval source.Interval, from, to time.Time) ([]source.Candle, error) {
	bar, ok := okxBars[interval]
	if !ok {
		return nil, unsupported(OKX, interval)
	}
	// after: older than this time; before: newer than this time.
	q := url.Values{"instId": {venueSymbol}, "bar": {bar}, "after": {strconv.FormatInt(to.UnixMilli(), 10)},
		"before": {strconv.FormatInt(from.UnixMilli()-1, 10)}, "limit": {strconv.Itoa(o.MaxCandles())}}
	var body struct {
		Code string  `json:"code"`
		Msg  string  `json:"msg"`
		Data [][]any `json:"data"`
	}
	if err := o.client.GetJSON(ctx, "api/v5/market/history-candles", q, &body); err != nil {
		return nil, err
	}
	if body.Code != "0" {
		return nil, fmt.Errorf("okx: %s %s", body.Code, body.Msg)
	}
	out := make([]source.Candle, 0, len(body.Data))
	for _, r := range body.Data {
		t, err := source.UnixMillis(at(r, 0))
		// The ninth figure is 1 for a closed candle.
		if err != nil || fmt.Sprint(at(r, 8)) == "0" {
			continue
		}
		if c, ok := ohlcv(t, at(r, 1), at(r, 2), at(r, 3), at(r, 4), at(r, 5)); ok {
			out = append(out, c)
		}
	}
	return closedIn(out, interval, from, to, o.now()), nil
}

// Kraken: /0/public/OHLC, which gives at most the last 720 candles of an
// interval, so its history goes back 720 candles only.

var krakenIntervals = map[source.Interval]string{source.Interval1m: "1", source.Interval5m: "5", source.Interval15m: "15",
	source.Interval1h: "60", source.Interval1d: "1440"}

func (k *kraken) MaxCandles() int { return 720 }

func (k *kraken) Supports(interval source.Interval) bool {
	_, ok := krakenIntervals[interval]
	return ok
}

func (k *kraken) Candles(ctx context.Context, venueSymbol string, interval source.Interval, from, to time.Time) ([]source.Candle, error) {
	minutes, ok := krakenIntervals[interval]
	if !ok {
		return nil, unsupported(Kraken, interval)
	}
	q := url.Values{"pair": {venueSymbol}, "interval": {minutes}, "since": {strconv.FormatInt(from.Unix()-1, 10)}}
	var body struct {
		Error  []string                   `json:"error"`
		Result map[string]json.RawMessage `json:"result"`
	}
	if err := k.client.GetJSON(ctx, "0/public/OHLC", q, &body); err != nil {
		return nil, err
	}
	if len(body.Error) > 0 {
		return nil, fmt.Errorf("kraken: %s", strings.Join(body.Error, "; "))
	}
	var out []source.Candle
	for name, raw := range body.Result {
		if name == "last" {
			continue
		}
		var rows [][]any
		if err := decodeNumbers(raw, &rows); err != nil {
			return nil, fmt.Errorf("kraken: candles of %s: %w", name, err)
		}
		for _, r := range rows {
			seconds := source.Int(at(r, 0))
			if seconds == nil {
				continue
			}
			// The seventh figure is the volume; the sixth is the VWAP.
			if c, ok := ohlcv(time.Unix(int64(*seconds), 0).UTC(), at(r, 1), at(r, 2), at(r, 3), at(r, 4), at(r, 6)); ok {
				out = append(out, c)
			}
		}
	}
	return closedIn(out, interval, from, to, k.now()), nil
}

// Coinbase: the public candles of a product, 350 per request, newest
// first.

var coinbaseGranularities = map[source.Interval]string{
	source.Interval1m: "ONE_MINUTE", source.Interval5m: "FIVE_MINUTE", source.Interval15m: "FIFTEEN_MINUTE",
	source.Interval1h: "ONE_HOUR", source.Interval1d: "ONE_DAY"}

func (c *coinbase) MaxCandles() int { return 350 }

func (c *coinbase) Supports(interval source.Interval) bool {
	_, ok := coinbaseGranularities[interval]
	return ok
}

func (c *coinbase) Candles(ctx context.Context, venueSymbol string, interval source.Interval, from, to time.Time) ([]source.Candle, error) {
	granularity, ok := coinbaseGranularities[interval]
	if !ok {
		return nil, unsupported(Coinbase, interval)
	}
	q := url.Values{"start": {strconv.FormatInt(from.Unix(), 10)}, "end": {strconv.FormatInt(to.Unix()-1, 10)},
		"granularity": {granularity}, "limit": {strconv.Itoa(c.MaxCandles())}}
	var body struct {
		Candles []struct {
			Start  any `json:"start"`
			Low    any `json:"low"`
			High   any `json:"high"`
			Open   any `json:"open"`
			Close  any `json:"close"`
			Volume any `json:"volume"`
		} `json:"candles"`
	}
	if err := c.client.GetJSON(ctx, "api/v3/brokerage/market/products/"+url.PathEscape(venueSymbol)+"/candles", q, &body); err != nil {
		return nil, err
	}
	out := make([]source.Candle, 0, len(body.Candles))
	for _, r := range body.Candles {
		seconds := source.Decimal(r.Start)
		if seconds == nil {
			continue
		}
		t := time.Unix(seconds.Decimal().IntPart(), 0).UTC()
		if cd, ok := ohlcv(t, r.Open, r.High, r.Low, r.Close, r.Volume); ok {
			out = append(out, cd)
		}
	}
	return closedIn(out, interval, from, to, c.now()), nil
}

// The Iranian venues answer the TradingView UDF history: arrays of times
// and figures. Their daily candles follow Tehran's day, not UTC's, so only
// the intraday resolutions are collected.

var udfResolutions = map[source.Interval]string{source.Interval1m: "1", source.Interval5m: "5", source.Interval15m: "15", source.Interval1h: "60"}

type udfHistory struct {
	S string `json:"s"`
	T []any  `json:"t"`
	O []any  `json:"o"`
	H []any  `json:"h"`
	L []any  `json:"l"`
	C []any  `json:"c"`
	V []any  `json:"v"`
}

func (u udfHistory) candles(venue string) ([]source.Candle, error) {
	switch u.S {
	case "ok":
	case "no_data":
		return nil, nil
	default:
		return nil, fmt.Errorf("%s: candle status %q", venue, u.S)
	}
	out := make([]source.Candle, 0, len(u.T))
	for i := range u.T {
		seconds := source.Int(u.T[i])
		if seconds == nil {
			continue
		}
		if c, ok := ohlcv(time.Unix(int64(*seconds), 0).UTC(), at(u.O, i), at(u.H, i), at(u.L, i), at(u.C, i), at(u.V, i)); ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func udfQuery(symbol, resolution string, from, to time.Time) url.Values {
	return url.Values{"symbol": {symbol}, "resolution": {resolution},
		"from": {strconv.FormatInt(from.Unix(), 10)}, "to": {strconv.FormatInt(to.Unix()-1, 10)}}
}

// Nobitex: /market/udf/history, whose symbols join the base and the quote
// in upper case, with IRT for the rial markets, and whose prices of those
// markets are in rials.

func (n *nobitex) MaxCandles() int { return 500 }

func (n *nobitex) Supports(interval source.Interval) bool {
	_, ok := udfResolutions[interval]
	return ok
}

func (n *nobitex) Candles(ctx context.Context, venueSymbol string, interval source.Interval, from, to time.Time) ([]source.Candle, error) {
	resolution, ok := udfResolutions[interval]
	baseSym, quoteSym, okSym := strings.Cut(venueSymbol, "-")
	if !ok || !okSym {
		return nil, unsupported(Nobitex, interval)
	}
	rials := strings.EqualFold(quoteSym, "rls")
	if rials {
		quoteSym = "IRT"
	}
	var body udfHistory
	if err := n.client.GetJSON(ctx, "market/udf/history", udfQuery(strings.ToUpper(baseSym+quoteSym), resolution, from, to), &body); err != nil {
		return nil, err
	}
	candles, err := body.candles(Nobitex)
	if err != nil {
		return nil, err
	}
	if rials {
		for i := range candles {
			c := &candles[i]
			c.Open, c.High, c.Low, c.Close = *tomans(&c.Open), *tomans(&c.High), *tomans(&c.Low), *tomans(&c.Close)
		}
	}
	return closedIn(candles, interval, from, to, n.now()), nil
}

// Wallex: /v1/udf/history, whose symbols are those of its markets.

func (w *wallex) MaxCandles() int { return 500 }

func (w *wallex) Supports(interval source.Interval) bool {
	_, ok := udfResolutions[interval]
	return ok
}

func (w *wallex) Candles(ctx context.Context, venueSymbol string, interval source.Interval, from, to time.Time) ([]source.Candle, error) {
	resolution, ok := udfResolutions[interval]
	if !ok {
		return nil, unsupported(Wallex, interval)
	}
	var body udfHistory
	if err := w.client.GetJSON(ctx, "v1/udf/history", udfQuery(venueSymbol, resolution, from, to), &body); err != nil {
		return nil, err
	}
	candles, err := body.candles(Wallex)
	if err != nil {
		return nil, err
	}
	return closedIn(candles, interval, from, to, w.now()), nil
}

// decodeNumbers decodes raw JSON with its numbers as json.Number, so that
// no figure passes through a float.
func decodeNumbers(raw json.RawMessage, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(out)
}
