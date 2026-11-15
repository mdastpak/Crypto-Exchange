package price

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/metrics"
)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return key
}

func pub(key ed25519.PrivateKey) ed25519.PublicKey { return key.Public().(ed25519.PublicKey) }

// fullPrice is a price with every field set.
func fullPrice(at time.Time) Price {
	p := livePrice("BTC/USDT", "100.5", at)
	p.Bid, p.Ask = sd("100.4"), sd("100.6")
	p.VWAP = map[string]shared.Decimal{"60s": sd("100.3"), "5s": sd("100.2")}
	p.TWAP = map[string]shared.Decimal{"300s": sd("100.1"), "60s": sd("100.0")}
	p.Outliers = []string{"okx"}
	return p
}

func TestSign_EveryFieldIsCovered(t *testing.T) {
	key := newKey(t)
	s, v := NewSigner(key), NewVerifier(pub(key))
	at := time.Date(2026, 11, 15, 12, 0, 0, 123456000, time.UTC)
	signed := s.Sign(fullPrice(at))
	assert.Equal(t, KeyID(pub(key)), signed.KeyID)
	assert.Empty(t, v.Verify(signed))

	// Through JSON, as Redis holds it.
	body, err := json.Marshal(signed)
	require.NoError(t, err)
	var read Price
	require.NoError(t, json.Unmarshal(body, &read))
	assert.Empty(t, v.Verify(read), "the encoding survives JSON")
	assert.Equal(t, canonical(signed), canonical(s.Sign(fullPrice(at))), "the encoding is stable")

	for name, change := range map[string]func(*Price){
		"market":   func(p *Price) { p.Market = "ETH/USDT" },
		"status":   func(p *Price) { p.Status = StatusStale },
		"bid":      func(p *Price) { p.Bid = sd("1") },
		"ask":      func(p *Price) { p.Ask = sd("1000") },
		"mid":      func(p *Price) { p.Mid = sd("1") },
		"vwap":     func(p *Price) { p.VWAP["60s"] = sd("1") },
		"twap":     func(p *Price) { p.TWAP["300s"] = sd("1") },
		"window":   func(p *Price) { p.TWAP["900s"] = sd("100") },
		"sources":  func(p *Price) { p.Sources = []string{"binance"} },
		"outliers": func(p *Price) { p.Outliers = nil },
		"at":       func(p *Price) { p.At = p.At.Add(time.Second) },
		"updated":  func(p *Price) { p.Updated = p.Updated.Add(time.Second) },
	} {
		p := s.Sign(fullPrice(at))
		change(&p)
		assert.Equal(t, ReasonBadSignature, v.Verify(p), name)
	}

	p := s.Sign(fullPrice(at))
	p.KeyID, p.Signature = "", ""
	assert.Equal(t, ReasonUnsigned, v.Verify(p))
	assert.Equal(t, ReasonUnknownKey, NewVerifier(pub(newKey(t))).Verify(s.Sign(fullPrice(at))))
	p = s.Sign(fullPrice(at))
	p.Signature = "not base64!"
	assert.Equal(t, ReasonMalformed, v.Verify(p))
	p.Signature = "AAAA"
	assert.Equal(t, ReasonMalformed, v.Verify(p), "too short")

	// A nil signer leaves the price unsigned.
	var none *Signer
	assert.Empty(t, none.Sign(fullPrice(at)).Signature)
}

func TestReader_RefusesAPriceNotSignedByTheAggregator(t *testing.T) {
	ctx := context.Background()
	client := testRedis(t)
	now := time.Now().UTC()
	oldKey, newKeyPair := newKey(t), newKey(t)
	r := NewReader(client, nil, time.Second).Verify(NewVerifier(pub(oldKey), pub(newKeyPair)))
	r.now = func() time.Time { return now }
	write := func(p Price) {
		body, err := json.Marshal(p)
		require.NoError(t, err)
		require.NoError(t, client.Set(ctx, Key(p.Market), body, time.Minute).Err())
	}

	// The publisher signs, and the reader takes it.
	pubOpts := newTestPublisher(client, nil)
	pubOpts.opts.Signer = NewSigner(oldKey)
	pubOpts.opts.Now = func() time.Time { return now }
	pubOpts.publish(ctx, []Price{livePrice("BTC/USDT", "100", now)})
	p, err := r.Get(ctx, "BTC/USDT")
	require.NoError(t, err)
	assert.Equal(t, "100", p.Mid.String())
	var entries int64
	entries, err = client.XLen(ctx, DefaultStream).Result()
	require.NoError(t, err)
	require.EqualValues(t, 1, entries)
	stream, err := client.XRange(ctx, DefaultStream, "-", "+").Result()
	require.NoError(t, err)
	u, err := DecodeUpdate([]byte(stream[0].Values["update"].(string)))
	require.NoError(t, err)
	assert.Empty(t, NewVerifier(pub(oldKey)).Verify(u.Prices[0]), "the stream carries the signature too")

	// A forged SET, fresh and live, is refused, and counted.
	before := testutil.ToFloat64(metrics.PriceSignatureFailures.WithLabelValues(ReasonUnsigned))
	write(livePrice("BTC/USDT", "1", now))
	_, err = r.Get(ctx, "BTC/USDT")
	assert.ErrorIs(t, err, ErrUnverifiedPrice)
	assert.ErrorIs(t, err, ErrStale, "a consumer refuses it as a stale price")
	assert.Equal(t, before+1, testutil.ToFloat64(metrics.PriceSignatureFailures.WithLabelValues(ReasonUnsigned)))

	// A signed price changed in Redis is refused.
	forged := NewSigner(oldKey).Sign(livePrice("BTC/USDT", "100", now))
	forged.Mid = sd("1")
	write(forged)
	_, err = r.Get(ctx, "BTC/USDT")
	assert.ErrorIs(t, err, ErrUnverifiedPrice)

	// A key that no server knows is refused.
	write(NewSigner(newKey(t)).Sign(livePrice("BTC/USDT", "100", now)))
	_, err = r.Get(ctx, "BTC/USDT")
	assert.ErrorIs(t, err, ErrUnverifiedPrice)

	// Not JSON at all.
	require.NoError(t, client.Set(ctx, Key("BTC/USDT"), "not json", time.Minute).Err())
	_, err = r.Get(ctx, "BTC/USDT")
	assert.ErrorIs(t, err, ErrUnverifiedPrice)

	// The rotation: the new key signs, and both are accepted meanwhile.
	write(NewSigner(newKeyPair).Sign(livePrice("BTC/USDT", "101", now)))
	p, err = r.Get(ctx, "BTC/USDT")
	require.NoError(t, err)
	assert.Equal(t, "101", p.Mid.String())
}

func TestLoadKeys(t *testing.T) {
	dir := t.TempDir()
	key := newKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	privPath := filepath.Join(dir, "price-signing.pem")
	require.NoError(t, os.WriteFile(privPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	pubDER, err := x509.MarshalPKIXPublicKey(pub(key))
	require.NoError(t, err)
	pubPath := filepath.Join(dir, "price-verify.pem")
	require.NoError(t, os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o600))

	loaded, err := LoadSigningKey(privPath)
	require.NoError(t, err)
	verify, err := LoadVerifyKey(pubPath)
	require.NoError(t, err)
	assert.Empty(t, NewVerifier(verify).Verify(NewSigner(loaded).Sign(fullPrice(time.Now()))))

	_, err = LoadSigningKey(pubPath)
	assert.Error(t, err, "a public key is not a signing key")
	_, err = LoadVerifyKey(privPath)
	assert.Error(t, err)
	_, err = LoadVerifyKey(filepath.Join(dir, "missing.pem"))
	assert.Error(t, err)
}
