package venues

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecret_IsNeverPrinted(t *testing.T) {
	c := Credentials{APIKey: NewSecret("key-123"), APISecret: NewSecret("secret-456"), Token: NewSecret("token-789")}
	b, err := json.Marshal(c)
	require.NoError(t, err)
	var logged bytes.Buffer
	log.New(&logged, "", 0).Printf("%v %+v %#v %s %q", c, c, c, c.APISecret, c.Token)
	for _, text := range []string{string(b), logged.String(), fmt.Sprint(c), fmt.Sprintf("%x", c.APIKey)} {
		for _, s := range []string{"key-123", "secret-456", "token-789"} {
			assert.NotContains(t, text, s)
		}
	}
	assert.Contains(t, string(b), redacted)
	assert.Equal(t, "secret-456", c.APISecret.Reveal())
}

func TestSecret_IsReadFromJSON(t *testing.T) {
	var c Credentials
	require.NoError(t, json.Unmarshal([]byte(`{"api_key":"k","api_secret":"s"}`), &c))
	assert.Equal(t, "k", c.APIKey.Reveal())
	assert.True(t, c.Token.Empty())
}

func TestFile_ReadsTheVenueAndAppliesAChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "venues.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"binance":{"api_key":"k1","api_secret":"s1"}}`), 0o600))
	f := File{Path: path}
	c, err := f.Credentials(context.Background(), "binance")
	require.NoError(t, err)
	assert.Equal(t, "k1", c.APIKey.Reveal())
	_, err = f.Credentials(context.Background(), "nobitex")
	assert.ErrorIs(t, err, ErrNoCredentials)
	require.NoError(t, os.WriteFile(path, []byte(`{"binance":{"api_key":"k2"}}`), 0o600))
	c, err = f.Credentials(context.Background(), "binance")
	require.NoError(t, err)
	assert.Equal(t, "k2", c.APIKey.Reveal())
}

// failing is a source that fails after its first answer.
type failing struct {
	calls int
	creds Credentials
}

func (f *failing) Credentials(context.Context, string) (Credentials, error) {
	f.calls++
	if f.calls > 1 {
		return Credentials{}, errors.New("away")
	}
	return f.creds, nil
}

func TestCached_KeepsTheLastWhileTheSourceIsAwayAndForgets(t *testing.T) {
	src := &failing{creds: Credentials{Token: NewSecret("t")}}
	now := time.Unix(1000, 0)
	c := NewCached(src, time.Minute)
	c.now = func() time.Time { return now }
	got, err := c.Credentials(context.Background(), "nobitex")
	require.NoError(t, err)
	assert.Equal(t, "t", got.Token.Reveal())
	_, _ = c.Credentials(context.Background(), "nobitex")
	assert.Equal(t, 1, src.calls, "within the TTL the source is not read")
	now = now.Add(2 * time.Minute)
	got, err = c.Credentials(context.Background(), "nobitex")
	require.NoError(t, err, "the last credentials are kept while the source is away")
	assert.Equal(t, "t", got.Token.Reveal())
	c.Forget("nobitex")
	_, err = c.Credentials(context.Background(), "nobitex")
	assert.Error(t, err, "forgotten credentials are read again")
}

func TestOpenBao_ReadsTheSecretOfTheVenue(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("bao-token-1\n"), 0o600))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "bao-token-1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/v1/kv/data/venues/binance":
			_, _ = w.Write([]byte(`{"data":{"data":{"api_key":"k","api_secret":"s"},"metadata":{"version":3}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	o, err := NewOpenBao(OpenBaoConfig{Address: srv.URL, TokenFile: tokenFile, Mount: "kv", Prefix: "venues/"})
	require.NoError(t, err)
	c, err := o.Credentials(context.Background(), "binance")
	require.NoError(t, err)
	assert.Equal(t, "s", c.APISecret.Reveal())
	_, err = o.Credentials(context.Background(), "nobitex")
	assert.ErrorIs(t, err, ErrNoCredentials)
	require.NoError(t, os.WriteFile(tokenFile, []byte("bao-token-2"), 0o600))
	_, err = o.Credentials(context.Background(), "binance")
	require.Error(t, err, "the token file is read on every request")
	assert.NotContains(t, err.Error(), "bao-token")

	_, err = NewOpenBao(OpenBaoConfig{Address: "ftp://x", Token: "t"})
	assert.Error(t, err)
	_, err = NewOpenBao(OpenBaoConfig{Address: srv.URL})
	assert.Error(t, err)
}

func TestMapping_MapsBothWaysAndRefusesTwoOfOne(t *testing.T) {
	m := Mapping{"USDT": {Currency: "USDT", Networks: map[string]string{"TRX": "tron", "ETH": "ethereum"}},
		"BTC": {Currency: "BTC", Networks: map[string]string{"BTC": "bitcoin"}}}
	require.NoError(t, m.Check())
	currency, network, ok := m.Ours("USDT", "TRX")
	assert.True(t, ok)
	assert.Equal(t, "USDT", currency)
	assert.Equal(t, "tron", network)
	_, _, ok = m.Ours("USDT", "BSC")
	assert.False(t, ok, "an unmapped network is not routable")
	_, _, ok = m.Ours("DOGE", "")
	assert.False(t, ok)
	asset, vn, ok := m.Venue("usdt", "ethereum")
	assert.True(t, ok)
	assert.Equal(t, "USDT", asset)
	assert.Equal(t, "ETH", vn)

	assert.Error(t, Mapping{"USDT": {Currency: "USDT"}, "USDT2": {Currency: "usdt"}}.Check())
	assert.Error(t, Mapping{"USDT": {Currency: "USDT", Networks: map[string]string{"TRX": "tron", "TRC20": "tron"}}}.Check())
	assert.Error(t, Mapping{"USDT": {}}.Check())
}

// counting is a venue that fails the test on any call that changes
// something.
type counting struct {
	Venue
	t *testing.T
}

func (c counting) Name() string             { return "inner" }
func (c counting) Supports(Capability) bool { return true }
func (c counting) PlaceOrder(context.Context, OrderRequest) (Order, error) {
	c.t.Fatal("the dry run sent an order")
	return Order{}, nil
}

func TestDryRun_SendsNothingAndKeepsWhatItLogged(t *testing.T) {
	var logged bytes.Buffer
	d := NewDryRun(counting{t: t}, log.New(&logged, "", 0))
	ctx := context.Background()
	o, err := d.PlaceOrder(ctx, OrderRequest{ClientID: "c1", Symbol: "BTCUSDT", Side: SideBuy, Type: OrderMarket,
		Quantity: shared.MustNewDecimalFromString("1")})
	require.NoError(t, err)
	assert.True(t, o.DryRun)
	assert.Equal(t, ModeDryRun, d.Mode())
	got, err := d.GetOrder(ctx, "BTCUSDT", "c1")
	require.NoError(t, err)
	assert.Equal(t, o, got)
	_, err = d.PlaceOrder(ctx, OrderRequest{ClientID: "c1"})
	var ve *Error
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, CodeDuplicate, ve.Code)
	_, err = d.GetOrder(ctx, "BTCUSDT", "c2")
	assert.ErrorIs(t, err, ErrNotFound)

	w, err := d.Withdraw(ctx, WithdrawRequest{ClientID: "w1", Asset: "USDT", Network: "TRX", Address: "TUserAddress123",
		Amount: shared.MustNewDecimalFromString("5")})
	require.NoError(t, err)
	assert.True(t, w.DryRun)
	assert.NotContains(t, logged.String(), "TUserAddress123", "the address is not logged")
	assert.Contains(t, logged.String(), "would withdraw")
}

func TestCaller_ClassifiesTheAnswers(t *testing.T) {
	status := http.StatusOK
	body := `{}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	client, err := source.NewClient(source.ClientOptions{Source: "venue_test", BaseURL: srv.URL, Timeout: time.Second, MaxAttempts: 1})
	require.NoError(t, err)
	c := Caller{Venue: "test", Client: client, Classify: func(status int, body []byte) (Kind, string) {
		if status == http.StatusRequestTimeout {
			return KindUnknown, "-1007"
		}
		return "", ""
	}}
	ctx := context.Background()
	write := source.Request{Method: http.MethodPost, Path: "x"}
	read := source.Request{Method: http.MethodGet, Path: "x", Idempotent: true}
	for _, tc := range []struct {
		status int
		body   string
		r      source.Request
		want   Kind
	}{
		{http.StatusBadRequest, `{}`, write, KindRejected},
		{http.StatusInternalServerError, `{}`, write, KindUnknown},
		{http.StatusInternalServerError, `{}`, read, KindRetryable},
		{http.StatusRequestTimeout, `{}`, write, KindUnknown},
		{http.StatusRequestTimeout, `{}`, read, KindRetryable},
		{http.StatusOK, `not json`, write, KindUnknown},
		{http.StatusOK, `not json`, read, KindRetryable},
	} {
		status, body = tc.status, tc.body
		var out map[string]any
		err := c.Call(ctx, "op", tc.r, &out)
		assert.Equal(t, tc.want, KindOf(err), "%d %s %v", tc.status, tc.r.Method, err)
	}
}
