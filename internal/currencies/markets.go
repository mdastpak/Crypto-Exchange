package currencies

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// PublicMarket is a market as users see it (#57): the pair, whether it
// trades, and its rules. Amounts are decimal strings.
type PublicMarket struct {
	// Symbol is the pair, base/quote.
	Symbol  string `json:"symbol" example:"BTC/USDT"`
	Base    string `json:"base" example:"BTC"`
	Quote   string `json:"quote" example:"USDT"`
	BaseHK  string `json:"base_hk" format:"uuid"`
	QuoteHK string `json:"quote_hk" format:"uuid"`
	// Status is ACTIVE while it trades; MAINTENANCE and SUSPENDED pause it.
	Status       string `json:"status" enums:"ACTIVE,MAINTENANCE,SUSPENDED" example:"ACTIVE"`
	MinOrderSize string `json:"min_order_size" example:"0.0001"`
	// MaxOrderSize of "0" is no maximum.
	MaxOrderSize string `json:"max_order_size" example:"0"`
	MinNotional  string `json:"min_notional" example:"10"`
	TickSize     string `json:"tick_size" example:"0.01"`
	LotSize      string `json:"lot_size" example:"0.0001"`
	// MakerFeeBPS and TakerFeeBPS are the fees in basis points: 10 is
	// 0.1%.
	MakerFeeBPS int `json:"maker_fee_bps" example:"10"`
	TakerFeeBPS int `json:"taker_fee_bps" example:"20"`
}

// DefaultMarketsTTL is how long an instance serves the markets it read
// before it reads them again. A change of a market reaches every instance
// within it.
const DefaultMarketsTTL = 10 * time.Second

// MarketList serves the public markets: those that trade or are paused, of
// two approved currencies, that are not deleted. Each instance keeps them in
// memory for its ttl, and reads them once for the requests that miss
// together. While the database cannot be read, it serves the markets it
// read last.
type MarketList struct {
	ttl  time.Duration
	now  func() time.Time
	load func(ctx context.Context) ([]PublicMarket, error)

	group singleflight.Group
	mu    sync.Mutex
	list  []PublicMarket
	at    time.Time
}

// NewMarketList returns the public markets of db, read at most every ttl,
// or DefaultMarketsTTL when it is not positive.
func NewMarketList(db *sql.DB, ttl time.Duration) *MarketList {
	if ttl <= 0 {
		ttl = DefaultMarketsTTL
	}
	return &MarketList{ttl: ttl, now: time.Now, load: func(ctx context.Context) ([]PublicMarket, error) {
		return loadPublicMarkets(ctx, db)
	}}
}

// List returns the public markets.
func (l *MarketList) List(ctx context.Context) ([]PublicMarket, error) {
	l.mu.Lock()
	list, at := l.list, l.at
	l.mu.Unlock()
	if list != nil && l.now().Sub(at) < l.ttl {
		return list, nil
	}
	value, err, _ := l.group.Do("markets", func() (any, error) {
		// One read serves the requests that wait for it, so it does not end
		// with the request that started it.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadTimeout)
		defer cancel()
		return l.load(ctx)
	})
	if err != nil {
		if list != nil {
			// The next read waits for the time again, so that an outage
			// costs one attempt and one line per ttl, not one a request.
			l.mu.Lock()
			l.at = l.now()
			l.mu.Unlock()
			log.Printf("Error reading the public markets; serving the ones read at %s: %v", at.UTC().Format(time.RFC3339), err)
			return list, nil
		}
		return nil, err
	}
	fresh := value.([]PublicMarket)
	l.mu.Lock()
	l.list, l.at = fresh, l.now()
	l.mu.Unlock()
	return fresh, nil
}

func loadPublicMarkets(ctx context.Context, db *sql.DB) ([]PublicMarket, error) {
	rows, err := db.QueryContext(ctx, `SELECT b.info->>'symbol', q.info->>'symbol', b.hk, q.hk, m.status::text,
			m.min_order_size::text, m.max_order_size::text, m.min_notional::text, m.tick_size::text, m.lot_size::text,
			m.maker_fee_bps, m.taker_fee_bps
		FROM markets m JOIN currencies b ON b.hk = m.base_currency_hk JOIN currencies q ON q.hk = m.quote_currency_hk
		WHERE m.deleted_at IS NULL AND m.status IN ('ACTIVE', 'MAINTENANCE', 'SUSPENDED')
			AND b.status = 'approved' AND b.deleted_at IS NULL AND q.status = 'approved' AND q.deleted_at IS NULL
		ORDER BY b.info->>'symbol', q.info->>'symbol'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []PublicMarket{}
	for rows.Next() {
		var m PublicMarket
		if err := rows.Scan(&m.Base, &m.Quote, &m.BaseHK, &m.QuoteHK, &m.Status, &m.MinOrderSize, &m.MaxOrderSize,
			&m.MinNotional, &m.TickSize, &m.LotSize, &m.MakerFeeBPS, &m.TakerFeeBPS); err != nil {
			return nil, err
		}
		m.Symbol = m.Base + "/" + m.Quote
		for _, amount := range []*string{&m.MinOrderSize, &m.MaxOrderSize, &m.MinNotional, &m.TickSize, &m.LotSize} {
			*amount = trimAmount(*amount)
		}
		list = append(list, m)
	}
	return list, rows.Err()
}
