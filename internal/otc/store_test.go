package otc

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"net"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"
	"github.com/mdastpak/crypto-exchange/internal/testutil/redistest"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDBName = "testdb_otc"

func testRedis(t testing.TB) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: redistest.Addr, DB: redistest.OTC})
	t.Cleanup(func() { client.Close() })
	if _, err := net.DialTimeout("tcp", redistest.Addr, time.Second); err != nil {
		t.Skipf("Redis is not reachable: %v", err)
	}
	require.NoError(t, client.FlushDB(context.Background()).Err())
	return client
}

// testDataKeys returns the data keys of a fresh database, as an instance
// loads them.
func testDataKeys(t testing.TB) *keys.Live {
	t.Helper()
	return dataKeysOn(t, pgtest.Migrated(t, testDBName))
}

// dataKeysOn returns the data keys of conn.
func dataKeysOn(t testing.TB, conn *sql.DB) *keys.Live {
	t.Helper()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	provider, err := keys.NewLocalProvider(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	live, err := keys.NewLive(context.Background(), &keys.Store{DB: conn, Provider: provider}, keys.LiveOptions{})
	require.NoError(t, err)
	return live
}

func newQuote(user string, ttl time.Duration) Quote {
	now := time.Now().UTC().Truncate(time.Millisecond)
	return Quote{
		ID: shared.NewUUID().String(), UserHK: user, Market: "BTC/USDT", Side: SideBuy,
		Quantity: shared.MustNewDecimalFromString("0.5"), Price: shared.MustNewDecimalFromString("100.5"),
		Total: shared.MustNewDecimalFromString("50.25"), Fee: shared.MustNewDecimalFromString("0.11"), FeeBPS: 20,
		FeeCurrency: "USDT", CreatedAt: now, ExpiresAt: now.Add(ttl),
		Components: Components{Reference: dec("100"), MarginBPS: dec("20")},
	}
}

// TestStore_TakesAQuoteOnceAndOnlyForItsAccount covers the acceptance
// criterion of #61: a foreign quote cannot be executed, and a quote is
// executed at most once.
func TestStore_TakesAQuoteOnceAndOnlyForItsAccount(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	store, err := NewStore(client, testDataKeys(t))
	require.NoError(t, err)
	alice, bob := shared.NewUUID().String(), shared.NewUUID().String()
	q := newQuote(alice, 8*time.Second)
	require.NoError(t, store.Save(ctx, q))

	ttl, err := client.PTTL(ctx, KeyPrefix+q.ID).Result()
	require.NoError(t, err)
	assert.InDelta(t, 8*time.Second, ttl, float64(time.Second), "the TTL is the expiry")

	_, err = store.Take(ctx, q.ID, bob, nil, time.Now())
	require.ErrorIs(t, err, ErrQuoteNotFound, "another user cannot take it")
	sub := shared.NewUUID().String()
	_, err = store.Take(ctx, q.ID, alice, &sub, time.Now())
	require.ErrorIs(t, err, ErrQuoteNotFound, "nor another account of the same user")

	got, err := store.Take(ctx, q.ID, alice, nil, time.Now())
	require.NoError(t, err, "a foreign take leaves the quote to its account")
	assert.Equal(t, q.ID, got.ID)
	assert.Equal(t, alice, got.UserHK)
	assert.True(t, q.Price.Equal(got.Price))
	assertDec(t, "20", got.Components.MarginBPS, "the parts come back for the execution")
	assert.True(t, q.ExpiresAt.Equal(got.ExpiresAt))

	_, err = store.Take(ctx, q.ID, alice, nil, time.Now())
	require.ErrorIs(t, err, ErrQuoteNotFound, "a quote is taken once")
}

// TestStore_RefusesAnExpiredQuote: Redis drops it at its expiry, and a
// take after the expiry that Redis has not dropped yet is refused too.
func TestStore_RefusesAnExpiredQuote(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	store, err := NewStore(client, nil)
	require.NoError(t, err)
	alice := shared.NewUUID().String()

	short := newQuote(alice, 150*time.Millisecond)
	require.NoError(t, store.Save(ctx, short))
	time.Sleep(300 * time.Millisecond)
	_, err = store.Take(ctx, short.ID, alice, nil, time.Now())
	require.ErrorIs(t, err, ErrQuoteNotFound)

	q := newQuote(alice, 5*time.Second)
	require.NoError(t, store.Save(ctx, q))
	_, err = store.Take(ctx, q.ID, alice, nil, q.ExpiresAt)
	require.ErrorIs(t, err, ErrQuoteExpired)

	require.ErrorIs(t, store.Save(ctx, newQuote(alice, -time.Second)), ErrQuoteExpired)
}

// TestStore_RefusesAQuoteChangedInRedis: a write to Redis cannot change the
// price, the owner, or the key version of a quote.
func TestStore_RefusesAQuoteChangedInRedis(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	store, err := NewStore(client, testDataKeys(t))
	require.NoError(t, err)
	alice, mallory := shared.NewUUID().String(), shared.NewUUID().String()

	for name, tamper := range map[string]func(key string, body string){
		"price": func(key, body string) {
			client.HSet(ctx, key, "body", body[:len(body)-1]+` `)
		},
		"owner": func(key, _ string) { client.HSet(ctx, key, "owner", mallory) },
		"mac":   func(key, _ string) { client.HSet(ctx, key, "mac", "00") },
		"version": func(key, _ string) {
			client.HSet(ctx, key, "v", "0")
		},
	} {
		q := newQuote(alice, 5*time.Second)
		require.NoError(t, store.Save(ctx, q), name)
		body, err := client.HGet(ctx, KeyPrefix+q.ID, "body").Result()
		require.NoError(t, err)
		tamper(KeyPrefix+q.ID, body)
		taker := alice
		if name == "owner" {
			taker = mallory
		}
		_, err = store.Take(ctx, q.ID, taker, nil, time.Now())
		require.ErrorIs(t, err, ErrQuoteTampered, name)
	}
}

// TestStore_AnotherInstanceTakesTheQuote: with data keys, a quote given on
// one instance is taken on another; with a key of the process, it is not.
func TestStore_AnotherInstanceTakesTheQuote(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	live := testDataKeys(t)
	one, err := NewStore(client, live)
	require.NoError(t, err)
	two, err := NewStore(client, live)
	require.NoError(t, err)
	alice := shared.NewUUID().String()
	q := newQuote(alice, 5*time.Second)
	require.NoError(t, one.Save(ctx, q))
	_, err = two.Take(ctx, q.ID, alice, nil, time.Now())
	require.NoError(t, err)

	local, err := NewStore(client, nil)
	require.NoError(t, err)
	other, err := NewStore(client, nil)
	require.NoError(t, err)
	q = newQuote(alice, 5*time.Second)
	require.NoError(t, local.Save(ctx, q))
	_, err = other.Take(ctx, q.ID, alice, nil, time.Now())
	require.ErrorIs(t, err, ErrQuoteTampered, "another process key does not match")
}
