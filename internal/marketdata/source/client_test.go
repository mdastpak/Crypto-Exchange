package source

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noSleep records the waits instead of sleeping.
type noSleep struct{ waits []time.Duration }

func (n *noSleep) sleep(ctx context.Context, d time.Duration) error {
	n.waits = append(n.waits, d)
	return ctx.Err()
}

func newTestClient(t *testing.T, url string, opts ClientOptions) (*Client, *noSleep) {
	t.Helper()
	opts.Source = "testsrc"
	opts.BaseURL = url + "/api/v3"
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	c, err := NewClient(opts)
	require.NoError(t, err)
	n := &noSleep{}
	c.sleep = n.sleep
	return c, n
}

func TestGetJSON_KeepsEveryDigitAndSendsTheHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v3/coins/markets", r.URL.Path)
		assert.Equal(t, "usd", r.URL.Query().Get("vs_currency"))
		assert.Equal(t, "secret-key", r.Header.Get("X-Api-Key"))
		assert.Equal(t, UserAgent, r.Header.Get("User-Agent"))
		_, _ = w.Write([]byte(`{"price": 0.000000012345678901, "supply": "21000000.000000000000000001"}`))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, ClientOptions{Header: http.Header{"X-Api-Key": {"secret-key"}}})

	var out map[string]any
	require.NoError(t, c.GetJSON(context.Background(), "coins/markets", map[string][]string{"vs_currency": {"usd"}}, &out))
	assert.Equal(t, "0.000000012345678901", Decimal(out["price"]).String())
	assert.Equal(t, "21000000.000000000000000001", Decimal(out["supply"]).String())
}

func TestGet_RetriesTransientAnswers(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	c, slept := newTestClient(t, srv.URL, ClientOptions{})

	_, err := c.Get(context.Background(), "ping", nil)
	require.NoError(t, err)
	assert.Equal(t, int32(3), calls.Load())
	require.Len(t, slept.waits, 2)
	assert.InDelta(t, 500*time.Millisecond, slept.waits[0], float64(100*time.Millisecond), "the backoff, with jitter")
	assert.Equal(t, 7*time.Second, slept.waits[1], "Retry-After wins")
}

func TestGet_DoesNotRetryWhatCannotSucceed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		check  func(t *testing.T, err error)
	}{
		{"not found", func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) },
			func(t *testing.T, err error) { assert.True(t, errors.Is(err, ErrNotFound)) }},
		{"refused", func(w http.ResponseWriter) { w.WriteHeader(http.StatusUnauthorized) },
			func(t *testing.T, err error) {
				var s *StatusError
				require.True(t, errors.As(err, &s))
				assert.Equal(t, http.StatusUnauthorized, s.Code)
			}},
		{"a long Retry-After", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
		}, func(t *testing.T, err error) { assert.ErrorContains(t, err, "asked to wait 1h0m0s") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				tc.answer(w)
			}))
			defer srv.Close()
			c, _ := newTestClient(t, srv.URL, ClientOptions{})
			_, err := c.Get(context.Background(), "x", nil)
			require.Error(t, err)
			tc.check(t, err)
			assert.Equal(t, int32(1), calls.Load())
		})
	}
}

func TestGet_RefusesAnOversizedAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 2048)))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, ClientOptions{MaxBodyBytes: 1024})
	_, err := c.Get(context.Background(), "big", nil)
	assert.ErrorContains(t, err, "more than 1024 bytes")
}

func TestGet_TheCircuitOpensAndProbes(t *testing.T) {
	var healthy atomic.Bool
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !healthy.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, ClientOptions{MaxAttempts: 1, BreakerFailures: 3, BreakerCooldown: time.Minute})
	now := time.Now()
	c.breaker.now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		_, err := c.Get(ctx, "x", nil)
		require.Error(t, err)
	}
	_, err := c.Get(ctx, "x", nil)
	assert.ErrorIs(t, err, ErrCircuitOpen)
	assert.Equal(t, int32(3), calls.Load(), "an open circuit makes no request")

	// After the cooldown one probe goes through; it fails, and the circuit
	// opens again.
	now = now.Add(61 * time.Second)
	_, err = c.Get(ctx, "x", nil)
	assert.NotErrorIs(t, err, ErrCircuitOpen)
	_, err = c.Get(ctx, "x", nil)
	assert.ErrorIs(t, err, ErrCircuitOpen)

	now = now.Add(61 * time.Second)
	healthy.Store(true)
	_, err = c.Get(ctx, "x", nil)
	require.NoError(t, err, "the probe succeeds and closes the circuit")
	_, err = c.Get(ctx, "x", nil)
	require.NoError(t, err)
}

func TestGet_ANotFoundDoesNotOpenTheCircuit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, ClientOptions{BreakerFailures: 2})
	for range 5 {
		_, err := c.Get(context.Background(), "x", nil)
		assert.ErrorIs(t, err, ErrNotFound)
	}
}

// limiterFunc is a Limiter of a function.
type limiterFunc func(ctx context.Context) error

func (f limiterFunc) Acquire(ctx context.Context) error { return f(ctx) }

func TestGet_AcquiresTheLimiterForEachAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	var acquired atomic.Int32
	c, _ := newTestClient(t, srv.URL, ClientOptions{Limiter: limiterFunc(func(ctx context.Context) error {
		acquired.Add(1)
		return nil
	})})
	_, err := c.Get(context.Background(), "x", nil)
	require.NoError(t, err)
	assert.Equal(t, int32(2), acquired.Load())

	exhausted, _ := newTestClient(t, srv.URL, ClientOptions{BreakerFailures: 1, Limiter: limiterFunc(func(ctx context.Context) error {
		return ErrBudgetExhausted
	})})
	_, err = exhausted.Get(context.Background(), "x", nil)
	assert.ErrorIs(t, err, ErrBudgetExhausted)
	_, err = exhausted.Get(context.Background(), "x", nil)
	assert.ErrorIs(t, err, ErrBudgetExhausted, "a used-up plan does not open the circuit")
}

func TestNewClient_RoutesThroughTheProxy(t *testing.T) {
	var proxied atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A proxy receives the absolute URL of the target.
		proxied.Store(r.URL.String())
		_ = json.NewEncoder(w).Encode(map[string]string{"via": "proxy"})
	}))
	defer proxy.Close()

	c, err := NewClient(ClientOptions{Source: "testsrc", BaseURL: "http://api.source.example/v1",
		Timeout: 5 * time.Second, ProxyURL: proxy.URL})
	require.NoError(t, err)
	var out map[string]string
	require.NoError(t, c.GetJSON(context.Background(), "ping", nil, &out))
	assert.Equal(t, "proxy", out["via"])
	assert.Equal(t, "http://api.source.example/v1/ping", proxied.Load())
}

func TestNewClient_NeverShowsTheProxyCredentials(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	c, err := NewClient(ClientOptions{Source: "testsrc", BaseURL: "http://api.source.example/v1",
		Timeout: 2 * time.Second, ProxyURL: "http://user:hunter2@" + addr, MaxAttempts: 1})
	require.NoError(t, err)
	_, err = c.Get(context.Background(), "ping", nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
	assert.NotContains(t, err.Error(), addr)

	_, err = NewClient(ClientOptions{Source: "testsrc", BaseURL: "https://x.example", Timeout: time.Second, ProxyURL: "ftp://user:hunter2@gw"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
}

func TestNewClient_IgnoresTheProxyOfTheEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, err := NewClient(ClientOptions{Source: "testsrc", BaseURL: srv.URL, Timeout: 5 * time.Second})
	require.NoError(t, err)
	_, err = c.Get(context.Background(), "x", nil)
	assert.NoError(t, err, "a direct route is direct, whatever the environment says")
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	d, ok := retryAfter("12", now)
	assert.True(t, ok)
	assert.Equal(t, 12*time.Second, d)
	d, ok = retryAfter(now.Add(30*time.Second).Format(http.TimeFormat), now)
	assert.True(t, ok)
	assert.Equal(t, 30*time.Second, d)
	_, ok = retryAfter("soon", now)
	assert.False(t, ok)
	_, ok = retryAfter("", now)
	assert.False(t, ok)
}

func TestDecodeHelpers(t *testing.T) {
	assert.Nil(t, Decimal(nil))
	assert.Nil(t, Decimal(""))
	assert.Nil(t, Decimal("n/a"))
	assert.Equal(t, "1.5", Decimal(" 1.5 ").String())
	_, err := RequiredDecimal("last", nil)
	assert.ErrorContains(t, err, "last")

	at, err := UnixMillis(json.Number("1762603200000"))
	require.NoError(t, err)
	assert.Equal(t, time.Date(2025, 11, 8, 12, 0, 0, 0, time.UTC), at)
	_, err = UnixMillis("abc")
	assert.Error(t, err)

	assert.Equal(t, 3, *Int(json.Number("3")))
	assert.Nil(t, Int(json.Number("0")))
	assert.Nil(t, Int(nil))
}
