package price

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// The consumer library (#59): the quote service and the other consumers
// read the current price of a market through a Reader, and the WebSocket
// gateway decodes the updates of the exchange with DecodeUpdate.

// The errors of a read. Both mean that there is no current price, and a
// consumer that prices something must refuse it.
var (
	// ErrNoPrice is a market without a price: none was ever written, or
	// the last one expired.
	ErrNoPrice = errors.New("price: no current price")
	// ErrStale is a price that is too old to be current.
	ErrStale = errors.New("price: the price is stale")

	errNotAnUpdate = errors.New("price: not a price update")
)

// StatusOverride is the status of a price that an administrator set (#59).
const StatusOverride Status = "override"

// Override is a price that an administrator set for a market until a time,
// in place of the aggregated one.
type Override struct {
	ID      string
	Market  string
	Bid     shared.Decimal
	Ask     shared.Decimal
	Mid     shared.Decimal
	Set     time.Time
	Expires time.Time
}

// Overrides are the overrides in force: *priceoverride.Watcher.
type Overrides interface {
	// Active returns the override of the market in force at now.
	Active(market string, now time.Time) (Override, bool)
}

// DefaultMaxAge is the default age past which a Reader refuses a price,
// even before its key expires.
const DefaultMaxAge = time.Second

// Reader reads the current prices.
type Reader struct {
	redis     redis.UniversalClient
	overrides Overrides
	maxAge    time.Duration
	now       func() time.Time
}

// NewReader returns a reader of the prices in client, which takes an
// administrator's override of overrides first; nil for none. A price
// computed more than maxAge ago is stale.
func NewReader(client redis.UniversalClient, overrides Overrides, maxAge time.Duration) *Reader {
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	return &Reader{redis: client, overrides: overrides, maxAge: maxAge, now: time.Now}
}

// Get returns the current price of the market: the override in force, or
// the aggregated price when it is live and fresh. It fails with ErrNoPrice
// or ErrStale when there is none, and with the error of Redis when it
// cannot read: a consumer never prices with anything else.
func (r *Reader) Get(ctx context.Context, market string) (Price, error) {
	now := r.now()
	if r.overrides != nil {
		if o, ok := r.overrides.Active(market, now); ok {
			return Price{
				Market: market, Status: StatusOverride, Bid: o.Bid, Ask: o.Ask, Mid: o.Mid,
				Sources: []string{"override"}, At: now.Truncate(time.Microsecond), Updated: o.Set,
			}, nil
		}
	}
	body, err := r.redis.Get(ctx, Key(market)).Bytes()
	if errors.Is(err, redis.Nil) {
		return Price{}, ErrNoPrice
	}
	if err != nil {
		return Price{}, fmt.Errorf("price: reading %s: %w", market, err)
	}
	var p Price
	if err := json.Unmarshal(body, &p); err != nil {
		return Price{}, fmt.Errorf("price: decoding %s: %w", market, err)
	}
	if p.Status != StatusLive || p.Market != market || !p.Mid.IsPositive() {
		return Price{}, ErrStale
	}
	if age := now.Sub(p.At); age > r.maxAge {
		return Price{}, ErrStale
	}
	return p, nil
}
