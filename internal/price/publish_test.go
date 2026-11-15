package price

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: redistest.Addr, DB: redistest.Price})
	t.Cleanup(func() { client.Close() })
	if _, err := net.DialTimeout("tcp", redistest.Addr, time.Second); err != nil {
		t.Skipf("Redis is not reachable: %v", err)
	}
	require.NoError(t, client.FlushDB(context.Background()).Err())
	return client
}

type fakeBroker struct {
	mu       sync.Mutex
	messages []amqp.Publishing
	fail     error
}

func (b *fakeBroker) PublishMessage(_ context.Context, exchange string, msg amqp.Publishing) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.messages = append(b.messages, msg)
	return nil
}

func (b *fakeBroker) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.messages)
}

func livePrice(market, mid string, at time.Time) Price {
	return Price{Market: market, Status: StatusLive, Bid: sd(mid), Ask: sd(mid), Mid: sd(mid), Sources: []string{"binance", "kraken"}, At: at, Updated: at}
}

func newTestPublisher(client redis.UniversalClient, broker Broker) *Publisher {
	return NewPublisher(PublisherOptions{Redis: client, CacheTTL: 300 * time.Millisecond, Stream: DefaultStream,
		Broker: broker, Exchange: "price_updates", LeaseTTL: time.Second, Logger: log.New(io.Discard, "", 0)})
}

// Every replica writes the live prices to their keys, with the TTL; a stale
// price is not written. One replica, the lease's holder, publishes the
// update to the stream and to RabbitMQ.
func TestPublisher_CacheStreamAndOneLeader(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	brokerA, brokerB := &fakeBroker{}, &fakeBroker{}
	a, b := newTestPublisher(client, brokerA), newTestPublisher(client, brokerB)
	now := time.Now().UTC()
	prices := []Price{livePrice("BTC/USDT", "100", now), {Market: "ETH/USDT", Status: StatusStale, At: now}}

	a.publish(ctx, prices)
	b.publish(ctx, prices)

	body, err := client.Get(ctx, Key("BTC/USDT")).Bytes()
	require.NoError(t, err)
	var cached Price
	require.NoError(t, json.Unmarshal(body, &cached))
	assert.Equal(t, "100", cached.Mid.String())
	ttl, err := client.PTTL(ctx, Key("BTC/USDT")).Result()
	require.NoError(t, err)
	assert.LessOrEqual(t, ttl, 300*time.Millisecond)
	assert.Positive(t, ttl)
	assert.Equal(t, int64(0), client.Exists(ctx, Key("ETH/USDT")).Val(), "a stale price is not written")

	assert.True(t, a.Leader())
	assert.False(t, b.Leader())
	assert.Equal(t, 1, brokerA.count())
	assert.Equal(t, 0, brokerB.count(), "one replica publishes")
	msg := brokerA.messages[0]
	assert.Equal(t, amqp.Transient, msg.DeliveryMode)
	assert.Equal(t, "2000", msg.Expiration)
	u, err := DecodeUpdate(msg.Body)
	require.NoError(t, err)
	assert.Len(t, u.Prices, 2, "the update holds every market, stale ones too")

	entries, err := client.XRange(ctx, DefaultStream, "-", "+").Result()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	u, err = DecodeUpdate([]byte(entries[0].Values["update"].(string)))
	require.NoError(t, err)
	assert.Equal(t, "BTC/USDT", u.Prices[0].Market)

	// The key expires once no replica writes it.
	time.Sleep(350 * time.Millisecond)
	assert.Equal(t, int64(0), client.Exists(ctx, Key("BTC/USDT")).Val())
}

// When the leader stops, it releases the lease, and another replica
// publishes at its next interval; when it dies, at the end of the lease.
func TestPublisher_Failover(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	brokerA, brokerB := &fakeBroker{}, &fakeBroker{}
	a, b := newTestPublisher(client, brokerA), newTestPublisher(client, brokerB)
	prices := []Price{livePrice("BTC/USDT", "100", time.Now())}

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { a.Run(runCtx); close(done) }()
	a.Prices(ctx, prices)
	require.Eventually(t, func() bool { return brokerA.count() == 1 }, time.Second, 5*time.Millisecond)
	b.publish(ctx, prices)
	assert.Equal(t, 0, brokerB.count())
	stop()
	<-done
	b.publish(ctx, prices)
	assert.Equal(t, 1, brokerB.count(), "the lease was released at the stop")

	// b dies without releasing: a takes over once the lease runs out.
	a.publish(ctx, prices)
	assert.Equal(t, 1, brokerA.count())
	time.Sleep(1100 * time.Millisecond)
	a.publish(ctx, prices)
	assert.Equal(t, 2, brokerA.count())
	assert.True(t, a.Leader())
}

// A failing broker does not stop the cache.
func TestPublisher_BrokerFailureKeepsTheCache(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	broker := &fakeBroker{fail: errors.New("connection refused")}
	p := newTestPublisher(client, broker)
	p.publish(ctx, []Price{livePrice("BTC/USDT", "100", time.Now())})
	assert.Equal(t, int64(1), client.Exists(ctx, Key("BTC/USDT")).Val())
}

// Prices never waits: it keeps the newest prices for Run.
func TestPublisher_PricesKeepsTheNewest(t *testing.T) {
	p := NewPublisher(PublisherOptions{})
	p.Prices(context.Background(), []Price{{Market: "A"}})
	p.Prices(context.Background(), []Price{{Market: "B"}})
	got := <-p.latest
	assert.Equal(t, "B", got[0].Market)
	assert.True(t, p.Leader(), "without Redis, the only replica publishes")
}

func TestLease_RenewsAndFences(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	clock := time.Now()
	a, b := NewLease(client, "lease:test", 900*time.Millisecond), NewLease(client, "lease:test", 900*time.Millisecond)
	a.now = func() time.Time { return clock }
	b.now = a.now
	held, err := a.Hold(ctx)
	require.NoError(t, err)
	assert.True(t, held)
	held, err = b.Hold(ctx)
	require.NoError(t, err)
	assert.False(t, held)

	// Within a third of the ttl, no call; after it, a renewal.
	clock = clock.Add(400 * time.Millisecond)
	held, err = a.Hold(ctx)
	require.NoError(t, err)
	assert.True(t, held)
	ttl := client.PTTL(ctx, "lease:test").Val()
	assert.Greater(t, ttl, 800*time.Millisecond, "renewed")

	// b cannot release a's lease.
	b.until = clock.Add(time.Second)
	b.Release(ctx)
	assert.Equal(t, a.token, client.Get(ctx, "lease:test").Val())
	a.Release(ctx)
	assert.Equal(t, int64(0), client.Exists(ctx, "lease:test").Val())
	assert.False(t, a.Held())
}

type fixedOverrides map[string]Override

func (f fixedOverrides) Active(market string, now time.Time) (Override, bool) {
	o, ok := f[market]
	return o, ok && now.Before(o.Expires)
}

// The reader serves a live, fresh price, the override in force first, and
// refuses everything else.
func TestReader(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	now := time.Now().UTC()
	write := func(p Price) {
		body, err := json.Marshal(p)
		require.NoError(t, err)
		require.NoError(t, client.Set(ctx, Key(p.Market), body, time.Minute).Err())
	}
	overrides := fixedOverrides{}
	r := NewReader(client, overrides, time.Second)
	r.now = func() time.Time { return now }

	_, err := r.Get(ctx, "BTC/USDT")
	assert.ErrorIs(t, err, ErrNoPrice)

	write(livePrice("BTC/USDT", "100", now.Add(-500*time.Millisecond)))
	p, err := r.Get(ctx, "BTC/USDT")
	require.NoError(t, err)
	assert.Equal(t, "100", p.Mid.String())

	write(livePrice("BTC/USDT", "100", now.Add(-2*time.Second)))
	_, err = r.Get(ctx, "BTC/USDT")
	assert.ErrorIs(t, err, ErrStale, "older than the max age")

	stale := livePrice("BTC/USDT", "100", now)
	stale.Status = StatusStale
	write(stale)
	_, err = r.Get(ctx, "BTC/USDT")
	assert.ErrorIs(t, err, ErrStale)

	other := livePrice("ETH/USDT", "100", now)
	body, _ := json.Marshal(other)
	require.NoError(t, client.Set(ctx, Key("BTC/USDT"), body, time.Minute).Err())
	_, err = r.Get(ctx, "BTC/USDT")
	assert.ErrorIs(t, err, ErrStale, "the key of another market")

	require.NoError(t, client.Set(ctx, Key("BTC/USDT"), "not json", time.Minute).Err())
	_, err = r.Get(ctx, "BTC/USDT")
	assert.Error(t, err)

	overrides["BTC/USDT"] = Override{Market: "BTC/USDT", Bid: sd("99"), Ask: sd("101"), Mid: sd("100"), Set: now.Add(-time.Hour), Expires: now.Add(time.Minute)}
	p, err = r.Get(ctx, "BTC/USDT")
	require.NoError(t, err)
	assert.Equal(t, StatusOverride, p.Status)
	assert.True(t, p.Live())
	assert.Equal(t, "99", p.Bid.String())
	assert.Equal(t, []string{"override"}, p.Sources)

	overrides["BTC/USDT"] = Override{Market: "BTC/USDT", Mid: sd("1"), Expires: now.Add(-time.Second)}
	_, err = r.Get(ctx, "BTC/USDT")
	assert.Error(t, err, "an expired override is not served")

	// Redis down: an error, never a price.
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 50 * time.Millisecond})
	defer down.Close()
	_, err = NewReader(down, nil, time.Second).Get(ctx, "BTC/USDT")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNoPrice)
}

func TestDecodeUpdate(t *testing.T) {
	_, err := DecodeUpdate([]byte(`{"event":"currency.updated"}`))
	assert.Error(t, err)
	_, err = DecodeUpdate([]byte(`nope`))
	assert.Error(t, err)
}
