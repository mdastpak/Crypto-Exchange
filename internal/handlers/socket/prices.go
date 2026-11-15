package socket

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/metrics"
	"github.com/mdastpak/crypto-exchange/internal/price"
)

// The price updates of the price engine (#59), which cmd/price-aggregator
// publishes to the price updates exchange beside the currency events.

const (
	// maxPriceUpdateBytes bounds an update from the broker: every market of
	// the platform in one message.
	maxPriceUpdateBytes = 1 << 20
	// maxPricesPerUpdate bounds the markets of one update.
	maxPricesPerUpdate = 2000
	// priceMinInterval is the least time between two sends of a market to
	// the clients, whatever the aggregator's interval.
	priceMinInterval = 100 * time.Millisecond
	// priceHeartbeat is the most time between two sends of a market whose
	// price does not change, so that a client sees it is still live.
	priceHeartbeat = 5 * time.Second
)

// PricesEvent is what the clients receive: the markets whose price changed,
// or that were not sent for a while.
type PricesEvent struct {
	Event      string        `json:"event" example:"prices.update"`
	Prices     []MarketPrice `json:"prices"`
	ServerTime string        `json:"server_time" example:"2026-11-15T10:00:00Z"`
}

// MarketPrice is the reference price of a market.
type MarketPrice struct {
	Market string         `json:"market" example:"BTC/USDT"`
	Bid    shared.Decimal `json:"bid" swaggertype:"string" example:"64000"`
	Ask    shared.Decimal `json:"ask" swaggertype:"string" example:"64100"`
	Mid    shared.Decimal `json:"mid" swaggertype:"string" example:"64050"`
	// Updated is when the newest provider input of the price was observed.
	Updated time.Time `json:"updated"`
}

// priceThrottle remembers what was sent of each market.
type priceThrottle struct {
	mu   sync.Mutex
	sent map[string]sentPrice
}

type sentPrice struct {
	bid, ask, mid shared.Decimal
	at            time.Time
}

// due reports whether p is sent at now, and records it when it is.
func (t *priceThrottle) due(p price.Price, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sent == nil {
		t.sent = map[string]sentPrice{}
	}
	last, ok := t.sent[p.Market]
	if ok {
		elapsed := now.Sub(last.at)
		if elapsed < priceMinInterval {
			return false
		}
		same := last.bid.Equal(p.Bid) && last.ask.Equal(p.Ask) && last.mid.Equal(p.Mid)
		if same && elapsed < priceHeartbeat {
			return false
		}
	}
	t.sent[p.Market] = sentPrice{bid: p.Bid, ask: p.Ask, mid: p.Mid, at: now}
	return true
}

// handlePriceUpdate forwards a price update from RabbitMQ to every
// connected client, rebuilt from its figures: only live prices of valid
// markets are kept, so that a stale price or a forged field never reaches a
// client.
func (h *WebSocketHandler) handlePriceUpdate(body []byte) {
	now := time.Now()
	data, latencies, err := h.prices.sanitize(body, now)
	if err != nil {
		log.Printf("Dropped a price update from the broker: %v", err)
		return
	}
	if data == nil {
		return
	}
	h.Broadcast(data)
	for _, l := range latencies {
		metrics.PriceDelivery.Observe(l.Seconds())
	}
}

// sanitize returns the event of an update for the clients, nil when no
// market is due, and the latency of each price sent.
func (t *priceThrottle) sanitize(body []byte, now time.Time) ([]byte, []time.Duration, error) {
	if len(body) > maxPriceUpdateBytes {
		return nil, nil, fmt.Errorf("the update has %d bytes, more than %d", len(body), maxPriceUpdateBytes)
	}
	u, err := price.DecodeUpdate(body)
	if err != nil {
		return nil, nil, fmt.Errorf("not a price update: %w", err)
	}
	if len(u.Prices) > maxPricesPerUpdate {
		return nil, nil, fmt.Errorf("the update has %d prices, more than %d", len(u.Prices), maxPricesPerUpdate)
	}
	event := PricesEvent{Event: price.EventPricesUpdate, Prices: []MarketPrice{}, ServerTime: now.UTC().Format(time.RFC3339)}
	var latencies []time.Duration
	for _, p := range u.Prices {
		market, err := price.NormalizeMarket(p.Market)
		if err != nil || market != p.Market || p.Status != price.StatusLive || !p.Mid.IsPositive() {
			continue
		}
		if !t.due(p, now) {
			continue
		}
		event.Prices = append(event.Prices, MarketPrice{Market: market, Bid: p.Bid, Ask: p.Ask, Mid: p.Mid, Updated: p.Updated.UTC()})
		if !p.Updated.IsZero() && !p.Updated.After(now) {
			latencies = append(latencies, now.Sub(p.Updated))
		}
	}
	if len(event.Prices) == 0 {
		return nil, nil, nil
	}
	data, err := json.Marshal(event)
	return data, latencies, err
}

// handleEvent forwards an event of the price updates exchange by its kind.
func (h *WebSocketHandler) handleEvent(body []byte) {
	var head struct {
		Event string `json:"event"`
	}
	if json.Unmarshal(body, &head) == nil && head.Event == price.EventPricesUpdate {
		h.handlePriceUpdate(body)
		return
	}
	h.handleCurrencyUpdate(body)
}
