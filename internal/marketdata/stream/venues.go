package stream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// The protocols are written from the venues' public documentation of
// their spot ticker streams; none needs a key. The Iranian venues have no
// public ticker stream that this follows, so their REST tickers stay.

// Protocols returns the protocol of each venue with a stream, by venue.
func Protocols() map[string]Protocol {
	return map[string]Protocol{
		"binance":  binance{},
		"okx":      okx{},
		"kraken":   kraken{},
		"coinbase": coinbase{},
	}
}

// decode reads a message keeping its numbers exact.
func decode(message []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(message))
	d.UseNumber()
	return d.Decode(out)
}

// positive returns the amount when it is a number above zero.
func positive(v any) *shared.Decimal {
	d := source.Decimal(v)
	if d == nil || !d.Decimal().IsPositive() {
		return nil
	}
	return d
}

// nonNegative returns the amount when it is a number of zero or more.
func nonNegative(v any) *shared.Decimal {
	d := source.Decimal(v)
	if d == nil || d.IsNegative() {
		return nil
	}
	return d
}

// ticker builds a ticker, or reports false without a positive last price.
func ticker(key string, last, bid, ask any, observed time.Time) (source.Ticker, bool) {
	price := positive(last)
	if price == nil {
		return source.Ticker{}, false
	}
	return source.Ticker{VenueSymbol: key, Last: *price, Bid: positive(bid), Ask: positive(ask), ObservedAt: observed}, true
}

// Binance: one SUBSCRIBE of the <symbol>@ticker streams, which push the
// rolling 24-hour ticker of each pair every second.
type binance struct{}

func (binance) Venue() string { return "binance" }
func (binance) URL() string   { return "wss://stream.binance.com:9443/ws" }
func (binance) Key(p marketdata.StreamPair) string {
	return strings.ToUpper(p.VenueSymbol)
}
func (binance) Ping() []byte { return nil }

func (binance) Subscribe(pairs []marketdata.StreamPair) ([][]byte, error) {
	streams := make([]string, len(pairs))
	for i, p := range pairs {
		streams[i] = strings.ToLower(p.VenueSymbol) + "@ticker"
	}
	m, err := json.Marshal(map[string]any{"method": "SUBSCRIBE", "params": streams, "id": 1})
	return [][]byte{m}, err
}

func (binance) Parse(message []byte, _ time.Time) ([]source.Ticker, error) {
	var m struct {
		Event  string `json:"e"`
		Time   any    `json:"E"`
		Symbol string `json:"s"`
		Last   any    `json:"c"`
		Bid    any    `json:"b"`
		Ask    any    `json:"a"`
		Open   any    `json:"o"`
		High   any    `json:"h"`
		Low    any    `json:"l"`
		Base   any    `json:"v"`
		Quote  any    `json:"q"`
	}
	if err := decode(message, &m); err != nil {
		return nil, fmt.Errorf("binance: %w", err)
	}
	if m.Event != "24hrTicker" {
		return nil, nil
	}
	observed, err := source.UnixMillis(m.Time)
	if err != nil {
		return nil, nil
	}
	t, ok := ticker(strings.ToUpper(m.Symbol), m.Last, m.Bid, m.Ask, observed)
	if !ok {
		return nil, nil
	}
	t.Open, t.High, t.Low = positive(m.Open), positive(m.High), positive(m.Low)
	t.VolumeBase, t.VolumeQuote = nonNegative(m.Base), nonNegative(m.Quote)
	return []source.Ticker{t}, nil
}

// OKX: the tickers channel of each instrument; the connection closes after
// 30 seconds without a message, so the text "ping" keeps it.
type okx struct{}

func (okx) Venue() string                      { return "okx" }
func (okx) URL() string                        { return "wss://ws.okx.com:8443/ws/v5/public" }
func (okx) Key(p marketdata.StreamPair) string { return p.VenueSymbol }
func (okx) Ping() []byte                       { return []byte("ping") }

func (okx) Subscribe(pairs []marketdata.StreamPair) ([][]byte, error) {
	args := make([]map[string]string, len(pairs))
	for i, p := range pairs {
		args[i] = map[string]string{"channel": "tickers", "instId": p.VenueSymbol}
	}
	m, err := json.Marshal(map[string]any{"op": "subscribe", "args": args})
	return [][]byte{m}, err
}

func (okx) Parse(message []byte, _ time.Time) ([]source.Ticker, error) {
	if string(message) == "pong" {
		return nil, nil
	}
	var m struct {
		Event string `json:"event"`
		Msg   string `json:"msg"`
		Arg   struct {
			Channel string `json:"channel"`
		} `json:"arg"`
		Data []struct {
			InstID string `json:"instId"`
			Last   any    `json:"last"`
			Bid    any    `json:"bidPx"`
			Ask    any    `json:"askPx"`
			Open   any    `json:"open24h"`
			High   any    `json:"high24h"`
			Low    any    `json:"low24h"`
			Base   any    `json:"vol24h"`
			Quote  any    `json:"volCcy24h"`
			Time   any    `json:"ts"`
		} `json:"data"`
	}
	if err := decode(message, &m); err != nil {
		return nil, fmt.Errorf("okx: %w", err)
	}
	if m.Event == "error" {
		return nil, fmt.Errorf("okx: %s", m.Msg)
	}
	if m.Arg.Channel != "tickers" {
		return nil, nil
	}
	var out []source.Ticker
	for _, d := range m.Data {
		observed, err := source.UnixMillis(d.Time)
		if err != nil {
			continue
		}
		t, ok := ticker(d.InstID, d.Last, d.Bid, d.Ask, observed)
		if !ok {
			continue
		}
		t.Open, t.High, t.Low = positive(d.Open), positive(d.High), positive(d.Low)
		t.VolumeBase, t.VolumeQuote = nonNegative(d.Base), nonNegative(d.Quote)
		out = append(out, t)
	}
	return out, nil
}

// Kraken: the ticker channel of WebSocket v2, which names the pairs BASE/QUOTE
// with BTC for XBT, as the pairs' symbols are stored. Its tickers carry no
// time, so they are observed when received.
type kraken struct{}

func (kraken) Venue() string { return "kraken" }
func (kraken) URL() string   { return "wss://ws.kraken.com/v2" }
func (kraken) Key(p marketdata.StreamPair) string {
	return p.BaseSymbol + "/" + p.QuoteSymbol
}
func (kraken) Ping() []byte { return nil }

func (k kraken) Subscribe(pairs []marketdata.StreamPair) ([][]byte, error) {
	symbols := make([]string, len(pairs))
	for i, p := range pairs {
		symbols[i] = k.Key(p)
	}
	m, err := json.Marshal(map[string]any{"method": "subscribe", "params": map[string]any{"channel": "ticker", "symbol": symbols}})
	return [][]byte{m}, err
}

func (kraken) Parse(message []byte, now time.Time) ([]source.Ticker, error) {
	var m struct {
		Method  string `json:"method"`
		Success *bool  `json:"success"`
		Error   string `json:"error"`
		Channel string `json:"channel"`
		Data    []struct {
			Symbol string `json:"symbol"`
			Last   any    `json:"last"`
			Bid    any    `json:"bid"`
			Ask    any    `json:"ask"`
			High   any    `json:"high"`
			Low    any    `json:"low"`
			Volume any    `json:"volume"`
			Change any    `json:"change"`
		} `json:"data"`
	}
	if err := decode(message, &m); err != nil {
		return nil, fmt.Errorf("kraken: %w", err)
	}
	if m.Success != nil && !*m.Success {
		// A symbol that Kraken does not stream is refused alone; the
		// others go on.
		return nil, nil
	}
	if m.Channel != "ticker" {
		return nil, nil
	}
	var out []source.Ticker
	for _, d := range m.Data {
		t, ok := ticker(d.Symbol, d.Last, d.Bid, d.Ask, now)
		if !ok {
			continue
		}
		t.High, t.Low, t.VolumeBase = positive(d.High), positive(d.Low), nonNegative(d.Volume)
		if change := source.Decimal(d.Change); change != nil {
			open := shared.Decimal(t.Last.Decimal().Sub(change.Decimal()))
			if open.Decimal().IsPositive() {
				t.Open = &open
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// Coinbase: the ticker channel of the Advanced Trade stream, with the
// heartbeats channel, which keeps the connection of quiet pairs open.
type coinbase struct{}

func (coinbase) Venue() string                      { return "coinbase" }
func (coinbase) URL() string                        { return "wss://advanced-trade-ws.coinbase.com" }
func (coinbase) Key(p marketdata.StreamPair) string { return p.VenueSymbol }
func (coinbase) Ping() []byte                       { return nil }

func (coinbase) Subscribe(pairs []marketdata.StreamPair) ([][]byte, error) {
	products := make([]string, len(pairs))
	for i, p := range pairs {
		products[i] = p.VenueSymbol
	}
	tickers, err := json.Marshal(map[string]any{"type": "subscribe", "product_ids": products, "channel": "ticker"})
	if err != nil {
		return nil, err
	}
	heartbeats, err := json.Marshal(map[string]any{"type": "subscribe", "channel": "heartbeats"})
	return [][]byte{tickers, heartbeats}, err
}

func (coinbase) Parse(message []byte, _ time.Time) ([]source.Ticker, error) {
	var m struct {
		Type      string `json:"type"`
		Message   string `json:"message"`
		Channel   string `json:"channel"`
		Timestamp string `json:"timestamp"`
		Events    []struct {
			Tickers []struct {
				Product string `json:"product_id"`
				Price   any    `json:"price"`
				Volume  any    `json:"volume_24_h"`
				Low     any    `json:"low_24_h"`
				High    any    `json:"high_24_h"`
				Bid     any    `json:"best_bid"`
				Ask     any    `json:"best_ask"`
			} `json:"tickers"`
		} `json:"events"`
	}
	if err := decode(message, &m); err != nil {
		return nil, fmt.Errorf("coinbase: %w", err)
	}
	if m.Type == "error" {
		return nil, fmt.Errorf("coinbase: %s", m.Message)
	}
	if m.Channel != "ticker" {
		return nil, nil
	}
	observed, err := time.Parse(time.RFC3339Nano, m.Timestamp)
	if err != nil {
		return nil, nil
	}
	var out []source.Ticker
	for _, e := range m.Events {
		for _, d := range e.Tickers {
			t, ok := ticker(d.Product, d.Price, d.Bid, d.Ask, observed.UTC())
			if !ok {
				continue
			}
			t.High, t.Low, t.VolumeBase = positive(d.High), positive(d.Low), nonNegative(d.Volume)
			out = append(out, t)
		}
	}
	return out, nil
}
