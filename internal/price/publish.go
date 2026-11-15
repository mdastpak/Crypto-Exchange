package price

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"strconv"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"

	"github.com/mdastpak/crypto-exchange/internal/metrics"
)

// The distribution of the prices (#59). Every replica of the aggregator
// writes the live price of each market to its Redis key, price:<market>,
// with a short TTL, so that a price that stops being written expires and is
// never served as current, and so that a replica that dies leaves no gap
// while another writes. One replica at a time, the holder of a lease in
// Redis, also publishes each interval's prices as one update to a Redis
// stream and to the RabbitMQ exchange of price updates, so that their
// consumers see one series.

// Defaults of the distribution.
const (
	DefaultCacheTTL     = 500 * time.Millisecond
	DefaultStream       = "price:stream"
	DefaultStreamMaxLen = 10_000
	DefaultLeaseTTL     = 2 * time.Second
	// LeaseKey is the key of the publisher's lease.
	LeaseKey = "price:publisher"
	// EventPricesUpdate is the event of an Update.
	EventPricesUpdate = "prices.update"
	// writeTimeout bounds the writes of one interval.
	writeTimeout = time.Second
)

// Key is the Redis key of the price of a market.
func Key(market string) string { return "price:" + market }

// Update is the message of one interval: the prices of every market, live
// and stale, on the stream and the exchange.
type Update struct {
	Event  string    `json:"event"`
	Prices []Price   `json:"prices"`
	At     time.Time `json:"at"`
}

// DecodeUpdate reads an update of the stream or the exchange.
func DecodeUpdate(body []byte) (Update, error) {
	var u Update
	if err := json.Unmarshal(body, &u); err != nil {
		return Update{}, err
	}
	if u.Event != EventPricesUpdate {
		return Update{}, errNotAnUpdate
	}
	return u, nil
}

// Broker publishes to RabbitMQ: *rabbitmq.RabbitMQService.
type Broker interface {
	PublishMessage(ctx context.Context, exchange string, msg amqp.Publishing) error
}

// PublisherOptions configure a publisher.
type PublisherOptions struct {
	// Redis holds the cache, the stream, and the lease; nil writes none.
	Redis redis.UniversalClient
	// CacheTTL is the life of a price's key.
	CacheTTL time.Duration
	// Stream is the Redis stream of the updates, capped at about
	// StreamMaxLen entries; an empty Stream publishes none.
	Stream       string
	StreamMaxLen int64
	// Broker and Exchange publish the updates to RabbitMQ; nil publishes
	// none. A message lives at most Expiration in a queue.
	Broker     Broker
	Exchange   string
	Expiration time.Duration
	// LeaseTTL is how long the lease of the publisher lasts unrenewed:
	// how soon another replica publishes after the holder dies.
	LeaseTTL time.Duration
	// Signer signs each price, in its key and in the updates (#450); nil
	// signs none, which a reader that verifies refuses.
	Signer *Signer
	Logger *log.Logger
	Now    func() time.Time
}

// Publisher is a Sink that distributes the prices. Its Prices never waits:
// the writes run in Run, which skips to the newest prices when a write is
// slow.
type Publisher struct {
	opts  PublisherOptions
	lease *Lease

	latest chan []Price
}

// NewPublisher returns a publisher. Run must run for it to publish.
func NewPublisher(opts PublisherOptions) *Publisher {
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	if opts.StreamMaxLen <= 0 {
		opts.StreamMaxLen = DefaultStreamMaxLen
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = DefaultLeaseTTL
	}
	if opts.Expiration <= 0 {
		opts.Expiration = 2 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	p := &Publisher{opts: opts, latest: make(chan []Price, 1)}
	if opts.Redis != nil {
		p.lease = NewLease(opts.Redis, LeaseKey, opts.LeaseTTL)
	}
	return p
}

// Prices implements Sink: it hands the prices to Run, replacing those that
// Run has not taken yet.
func (p *Publisher) Prices(_ context.Context, prices []Price) {
	select {
	case <-p.latest:
	default:
	}
	p.latest <- prices
}

// Leader reports whether this replica publishes the updates.
func (p *Publisher) Leader() bool { return p.lease == nil || p.lease.Held() }

// Run writes the prices until ctx ends, and then gives up the lease.
func (p *Publisher) Run(ctx context.Context) {
	defer func() {
		if p.lease != nil {
			releaseCtx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			p.lease.Release(releaseCtx)
			cancel()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case prices := <-p.latest:
			p.publish(ctx, prices)
		}
	}
}

func (p *Publisher) publish(ctx context.Context, prices []Price) {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if p.opts.Signer != nil {
		signed := make([]Price, len(prices))
		for i, price := range prices {
			signed[i] = p.opts.Signer.Sign(price)
		}
		prices = signed
	}
	now := p.opts.Now()
	if p.opts.Redis != nil {
		if err := p.cache(ctx, prices); err != nil {
			metrics.PricePublishErrors.WithLabelValues("cache").Inc()
			p.opts.Logger.Printf("price publisher: writing the cache: %v", err)
		}
	}
	leader := true
	if p.lease != nil {
		var err error
		if leader, err = p.lease.Hold(ctx); err != nil {
			metrics.PricePublishErrors.WithLabelValues("lease").Inc()
			p.opts.Logger.Printf("price publisher: the lease: %v", err)
		}
	}
	metrics.PricePublisherLeader.Set(boolGauge(leader))
	if !leader {
		return
	}
	body, err := json.Marshal(Update{Event: EventPricesUpdate, Prices: prices, At: now.Truncate(time.Microsecond)})
	if err != nil {
		p.opts.Logger.Printf("price publisher: encoding: %v", err)
		return
	}
	if p.opts.Redis != nil && p.opts.Stream != "" {
		err := p.opts.Redis.XAdd(ctx, &redis.XAddArgs{
			Stream: p.opts.Stream, MaxLen: p.opts.StreamMaxLen, Approx: true, Values: map[string]any{"update": body},
		}).Err()
		if err != nil {
			metrics.PricePublishErrors.WithLabelValues("stream").Inc()
			p.opts.Logger.Printf("price publisher: the stream: %v", err)
		}
	}
	if p.opts.Broker != nil && p.opts.Exchange != "" {
		// Transient, and dropped from a queue once old: a consumer that
		// falls behind wants the next prices, not the old ones.
		err := p.opts.Broker.PublishMessage(ctx, p.opts.Exchange, amqp.Publishing{
			ContentType: "application/json", Body: body, DeliveryMode: amqp.Transient,
			Expiration: strconv.FormatInt(p.opts.Expiration.Milliseconds(), 10),
		})
		if err != nil {
			metrics.PricePublishErrors.WithLabelValues("rabbitmq").Inc()
			p.opts.Logger.Printf("price publisher: RabbitMQ: %v", err)
		}
	}
}

// cache writes the live prices to their keys. A stale price is not written:
// its key expires within the TTL.
func (p *Publisher) cache(ctx context.Context, prices []Price) error {
	pipe := p.opts.Redis.Pipeline()
	n := 0
	for _, price := range prices {
		if !price.Live() {
			continue
		}
		body, err := json.Marshal(price)
		if err != nil {
			return err
		}
		pipe.Set(ctx, Key(price.Market), body, p.opts.CacheTTL)
		n++
	}
	if n == 0 {
		return nil
	}
	_, err := pipe.Exec(ctx)
	return err
}

// Lease is a lease in Redis that one holder at a time keeps by renewing it,
// with a token of its own, so that a holder never renews or releases
// another's lease.
type Lease struct {
	client redis.UniversalClient
	key    string
	ttl    time.Duration
	token  string
	now    func() time.Time

	mu sync.Mutex
	// until is when the holder must renew to be sure it still holds it.
	until time.Time
}

// NewLease returns the lease of key, of ttl.
func NewLease(client redis.UniversalClient, key string, ttl time.Duration) *Lease {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return &Lease{client: client, key: key, ttl: ttl, token: hex.EncodeToString(b), now: time.Now}
}

// renew extends the lease when the token holds it.
var renew = redis.NewScript(`if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0`)

// release deletes the lease when the token holds it.
var release = redis.NewScript(`if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0`)

// Held reports whether the lease was held at its last renewal, and is not
// due for another.
func (l *Lease) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.now().Before(l.until)
}

// Hold takes the lease, or renews it, at most every third of its ttl, and
// reports whether this holder has it.
func (l *Lease) Hold(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Before(l.until.Add(-2 * l.ttl / 3)) {
		return true, nil
	}
	held := false
	if now.Before(l.until) {
		n, err := renew.Run(ctx, l.client, []string{l.key}, l.token, l.ttl.Milliseconds()).Int()
		if err != nil {
			return now.Before(l.until), err
		}
		held = n == 1
	}
	if !held {
		ok, err := l.client.SetNX(ctx, l.key, l.token, l.ttl).Result()
		if err != nil {
			l.until = time.Time{}
			return false, err
		}
		held = ok
	}
	if !held {
		l.until = time.Time{}
		return false, nil
	}
	// The lease runs from before the call, so that this holder stops
	// believing it holds it before Redis lets it go.
	l.until = now.Add(l.ttl)
	return true, nil
}

// Release gives up the lease, so that another replica publishes at once.
func (l *Lease) Release(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.until.IsZero() {
		return
	}
	_ = release.Run(ctx, l.client, []string{l.key}, l.token).Err()
	l.until = time.Time{}
}
