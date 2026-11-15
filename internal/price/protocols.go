package price

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// The protocols are written from the providers' public documentation of
// their spot streams; none needs a key.

// Protocols returns the protocol of each provider with a stream, by name.
func Protocols() map[string]Protocol {
	return map[string]Protocol{
		"binance":  binance{},
		"coinbase": coinbase{},
		"kraken":   kraken{},
	}
}

// decode reads a message keeping its numbers exact.
func decode(message []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(message))
	d.UseNumber()
	return d.Decode(out)
}

// amount returns a number of a message, or zero when it is not one; the
// aggregator rejects the ticks with an amount that is not positive.
func amount(v any) shared.Decimal {
	if d := source.Decimal(v); d != nil {
		return *d
	}
	return shared.Decimal{}
}

// Binance: the <symbol>@bookTicker stream, which pushes every change of the
// best bid and ask, and the <symbol>@trade stream, on one connection.
// Binance pings the connection itself; the dialer answers.
type binance struct{}

func (binance) URL() string { return "wss://stream.binance.com:9443/ws" }

func (binance) Native(market string) string {
	base, quote := SplitMarket(market)
	return base + quote
}

func (binance) Ping() []byte { return nil }

func (binance) Subscribe(natives []string) ([][]byte, error) {
	streams := make([]string, 0, 2*len(natives))
	for _, n := range natives {
		s := strings.ToLower(n)
		streams = append(streams, s+"@bookTicker", s+"@trade")
	}
	m, err := json.Marshal(map[string]any{"method": "SUBSCRIBE", "params": streams, "id": 1})
	return [][]byte{m}, err
}

func (binance) Parse(message []byte, received time.Time) ([]Tick, error) {
	// A map, not a struct: the keys differ only in case ("b" is the bid
	// and "B" its quantity), and a struct would take either.
	var m map[string]any
	if err := decode(message, &m); err != nil {
		return nil, fmt.Errorf("binance: %w", err)
	}
	if _, failed := m["code"]; failed {
		return nil, fmt.Errorf("binance: %v", m["msg"])
	}
	symbol, _ := m["s"].(string)
	symbol = strings.ToUpper(symbol)
	event, _ := m["e"].(string)
	_, isBook := m["u"]
	switch {
	case event == "trade":
		at, err := source.UnixMillis(m["T"])
		if err != nil {
			at = time.Time{}
		}
		return []Tick{{Market: symbol, Kind: KindTrade, Price: amount(m["p"]), Size: amount(m["q"]), At: at, Received: received}}, nil
	case event == "" && isBook && symbol != "":
		// A book ticker carries no time: it is observed when received.
		return []Tick{{Market: symbol, Kind: KindBook, Bid: amount(m["b"]), Ask: amount(m["a"]), Received: received}}, nil
	}
	return nil, nil
}

// Coinbase: the ticker channel of the Exchange feed, whose every message is
// a trade with the best bid and ask after it, and the heartbeat channel,
// which keeps the connection of quiet markets open.
type coinbase struct{}

func (coinbase) URL() string { return "wss://ws-feed.exchange.coinbase.com" }

func (coinbase) Native(market string) string {
	base, quote := SplitMarket(market)
	return base + "-" + quote
}

func (coinbase) Ping() []byte { return nil }

func (coinbase) Subscribe(natives []string) ([][]byte, error) {
	m, err := json.Marshal(map[string]any{"type": "subscribe", "product_ids": natives, "channels": []string{"ticker", "heartbeat"}})
	return [][]byte{m}, err
}

func (coinbase) Parse(message []byte, received time.Time) ([]Tick, error) {
	var m struct {
		Type     string `json:"type"`
		Message  string `json:"message"`
		Reason   string `json:"reason"`
		Product  string `json:"product_id"`
		Price    any    `json:"price"`
		Bid      any    `json:"best_bid"`
		Ask      any    `json:"best_ask"`
		LastSize any    `json:"last_size"`
		TradeID  any    `json:"trade_id"`
		Time     string `json:"time"`
	}
	if err := decode(message, &m); err != nil {
		return nil, fmt.Errorf("coinbase: %w", err)
	}
	if m.Type == "error" {
		return nil, fmt.Errorf("coinbase: %s %s", m.Message, m.Reason)
	}
	if m.Type != "ticker" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339Nano, m.Time)
	if err != nil {
		at = time.Time{}
	}
	ticks := []Tick{{Market: m.Product, Kind: KindBook, Bid: amount(m.Bid), Ask: amount(m.Ask), At: at, Received: received}}
	if m.TradeID != nil {
		ticks = append(ticks, Tick{Market: m.Product, Kind: KindTrade, Price: amount(m.Price), Size: amount(m.LastSize), At: at, Received: received})
	}
	return ticks, nil
}

// Kraken: the ticker channel of WebSocket v2 on every change of the best
// bid and ask, and the trade channel without its snapshot, so that a
// reconnection does not count the same trades again. Kraken names the
// markets BASE/QUOTE, with BTC for XBT.
type kraken struct{}

func (kraken) URL() string                 { return "wss://ws.kraken.com/v2" }
func (kraken) Native(market string) string { return market }
func (kraken) Ping() []byte                { return nil }

func (kraken) Subscribe(natives []string) ([][]byte, error) {
	book, err := json.Marshal(map[string]any{"method": "subscribe", "params": map[string]any{
		"channel": "ticker", "symbol": natives, "event_trigger": "bbo"}})
	if err != nil {
		return nil, err
	}
	trades, err := json.Marshal(map[string]any{"method": "subscribe", "params": map[string]any{
		"channel": "trade", "symbol": natives, "snapshot": false}})
	return [][]byte{book, trades}, err
}

func (kraken) Parse(message []byte, received time.Time) ([]Tick, error) {
	var m struct {
		Method  string `json:"method"`
		Success *bool  `json:"success"`
		Error   string `json:"error"`
		Channel string `json:"channel"`
		Data    []struct {
			Symbol    string `json:"symbol"`
			Bid       any    `json:"bid"`
			Ask       any    `json:"ask"`
			Price     any    `json:"price"`
			Qty       any    `json:"qty"`
			Timestamp string `json:"timestamp"`
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
	var out []Tick
	for _, d := range m.Data {
		symbol := d.Symbol
		if n, err := NormalizeMarket(symbol); err == nil {
			symbol = n
		}
		at, err := time.Parse(time.RFC3339Nano, d.Timestamp)
		if err != nil {
			at = time.Time{}
		}
		switch m.Channel {
		case "ticker":
			out = append(out, Tick{Market: symbol, Kind: KindBook, Bid: amount(d.Bid), Ask: amount(d.Ask), At: at, Received: received})
		case "trade":
			out = append(out, Tick{Market: symbol, Kind: KindTrade, Price: amount(d.Price), Size: amount(d.Qty), At: at, Received: received})
		}
	}
	return out, nil
}
