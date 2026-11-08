package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UserAgent identifies the worker to the sources, without personal data.
const UserAgent = "crypto-exchange-market-worker/1"

// Defaults of a Client.
const (
	DefaultMaxAttempts     = 4
	DefaultMaxBodyBytes    = 16 << 20
	DefaultBreakerFailures = 5
	DefaultBreakerCooldown = time.Minute
	// maxRetryAfter bounds the wait that a Retry-After header asks for, so
	// that a run is not held for long; a longer wait fails the request,
	// and the run backs off.
	maxRetryAfter = 60 * time.Second
	// maxBackoff bounds the wait between two attempts.
	maxBackoff = 10 * time.Second
)

// ErrCircuitOpen is returned without a request while a source that kept
// failing is left alone.
var ErrCircuitOpen = errors.New("source: the source failed repeatedly and is left alone for a while")

// StatusError is an answer of a source that is not 2xx.
type StatusError struct {
	Source string
	Path   string
	Code   int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("source %s: %s answered %d", e.Source, e.Path, e.Code)
}

// Limiter keeps the requests of a source within the limits of its plan.
type Limiter interface {
	// Acquire waits until a request may be made, and returns
	// ErrBudgetExhausted when the plan has no request left.
	Acquire(ctx context.Context) error
}

// ClientOptions configure a Client.
type ClientOptions struct {
	// Source names the source in errors and logs.
	Source string
	// BaseURL is the address of the API, such as
	// https://api.coingecko.com/api/v3.
	BaseURL string
	// Timeout bounds one request.
	Timeout time.Duration
	// ProxyURL routes the requests through a proxy (http, https, or
	// socks5); empty is direct. It never appears in errors.
	ProxyURL string
	// Header is sent with every request, such as the API key of the plan.
	// Keys never go in the URL, so that no log or error holds them.
	Header http.Header
	// Limiter, when set, is acquired before every attempt.
	Limiter Limiter
	// MaxAttempts bounds the attempts of one request.
	MaxAttempts int
	// MaxBodyBytes bounds a response.
	MaxBodyBytes int64
	// BreakerFailures failed requests in a row open the circuit for
	// BreakerCooldown.
	BreakerFailures int
	BreakerCooldown time.Duration

	// Transport replaces the HTTP transport, in tests.
	Transport http.RoundTripper
}

// Client calls the API of one source. It is safe for concurrent use.
type Client struct {
	opts    ClientOptions
	base    *url.URL
	http    *http.Client
	breaker breaker
	sleep   func(ctx context.Context, d time.Duration) error
}

// NewClient returns a client of the source.
func NewClient(opts ClientOptions) (*Client, error) {
	base, err := url.Parse(opts.BaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, fmt.Errorf("source %s: invalid base URL %q", opts.Source, opts.BaseURL)
	}
	if opts.Timeout <= 0 {
		return nil, fmt.Errorf("source %s: the timeout must be positive", opts.Source)
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = DefaultMaxAttempts
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if opts.BreakerFailures <= 0 {
		opts.BreakerFailures = DefaultBreakerFailures
	}
	if opts.BreakerCooldown <= 0 {
		opts.BreakerCooldown = DefaultBreakerCooldown
	}
	transport := opts.Transport
	if transport == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		// The node's configuration alone decides the route; the proxy
		// variables of the environment are ignored.
		t.Proxy = nil
		if opts.ProxyURL != "" {
			proxy, err := url.Parse(opts.ProxyURL)
			if err != nil || proxy.Host == "" {
				return nil, fmt.Errorf("source %s: invalid proxy URL", opts.Source)
			}
			switch proxy.Scheme {
			case "http", "https", "socks5":
			default:
				return nil, fmt.Errorf("source %s: the proxy must be http, https, or socks5", opts.Source)
			}
			t.Proxy = http.ProxyURL(proxy)
		}
		transport = t
	}
	return &Client{
		opts:    opts,
		base:    base,
		http:    &http.Client{Transport: transport, Timeout: opts.Timeout},
		breaker: breaker{failures: opts.BreakerFailures, cooldown: opts.BreakerCooldown, now: time.Now},
		sleep:   sleepCtx,
	}, nil
}

// GetJSON gets path, relative to the base URL, with query, and decodes the
// JSON answer into out, keeping numbers as json.Number so that no amount
// passes through a float. It retries network errors, 429, and 5xx answers,
// waiting as Retry-After asks or with a backoff; a 404 is ErrNotFound.
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, out any) error {
	body, err := c.Get(ctx, path, query)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("source %s: decoding %s: %w", c.opts.Source, path, err)
	}
	return nil
}

// Get gets path and returns the body of a 2xx answer.
func (c *Client) Get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	if !c.breaker.allow() {
		return nil, fmt.Errorf("source %s: %w", c.opts.Source, ErrCircuitOpen)
	}
	target := c.base.JoinPath(path)
	target.RawQuery = query.Encode()

	var lastErr error
	for attempt := 1; attempt <= c.opts.MaxAttempts; attempt++ {
		if c.opts.Limiter != nil {
			if err := c.opts.Limiter.Acquire(ctx); err != nil {
				// A plan without requests left is not a failure of the
				// source; the circuit stays as it is.
				return nil, fmt.Errorf("source %s: %w", c.opts.Source, err)
			}
		}
		body, wait, err := c.attempt(ctx, target, path)
		if err == nil {
			c.breaker.success()
			return body, nil
		}
		lastErr = err
		if wait < 0 || attempt == c.opts.MaxAttempts || ctx.Err() != nil {
			break
		}
		if wait == 0 {
			wait = backoff(attempt)
		}
		if err := c.sleep(ctx, wait); err != nil {
			lastErr = err
			break
		}
	}
	switch {
	case countsAgainst(lastErr):
		c.breaker.failure()
	case errors.Is(lastErr, context.Canceled), errors.Is(lastErr, context.DeadlineExceeded) && ctx.Err() != nil:
		// Our own cancellation says nothing of the source.
		c.breaker.release()
	default:
		// The source answered, if only with a 404 or a 4xx.
		c.breaker.success()
	}
	return nil, lastErr
}

// attempt makes one request. wait is how long to wait before the next
// attempt: 0 for the backoff, a Retry-After, or -1 when another attempt is
// pointless.
func (c *Client) attempt(ctx context.Context, target *url.URL, path string) (body []byte, wait time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, -1, err
	}
	for k, vs := range c.opts.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// The error of the client names the URL, which holds no key; it
		// may name the proxy, so only the kind of failure is kept.
		return nil, 0, fmt.Errorf("source %s: %s: %w", c.opts.Source, path, networkError(err))
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		body, err := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxBodyBytes+1))
		if err != nil {
			return nil, 0, fmt.Errorf("source %s: reading %s: %w", c.opts.Source, path, networkError(err))
		}
		if int64(len(body)) > c.opts.MaxBodyBytes {
			return nil, -1, fmt.Errorf("source %s: %s answered more than %d bytes", c.opts.Source, path, c.opts.MaxBodyBytes)
		}
		return body, 0, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, -1, fmt.Errorf("source %s: %s: %w", c.opts.Source, path, ErrNotFound)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		statusErr := &StatusError{Source: c.opts.Source, Path: path, Code: resp.StatusCode}
		after, ok := retryAfter(resp.Header.Get("Retry-After"), time.Now())
		if ok && after > maxRetryAfter {
			return nil, -1, fmt.Errorf("%w, and asked to wait %s", statusErr, after.Round(time.Second))
		}
		return nil, after, statusErr
	default:
		return nil, -1, &StatusError{Source: c.opts.Source, Path: path, Code: resp.StatusCode}
	}
}

// countsAgainst reports whether err is a failure of the source, which
// counts toward opening the circuit: not an unknown asset, a refused
// request of ours, or our own cancellation.
func countsAgainst(err error) bool {
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, context.Canceled) {
		return false
	}
	var status *StatusError
	if errors.As(err, &status) {
		return status.Code == http.StatusTooManyRequests || status.Code >= 500
	}
	return true
}

// networkError keeps the kind of a transport error without its text, which
// may name the proxy and its credentials.
func networkError(err error) error {
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.As(err, &netErr) && netErr.Timeout():
		return errors.New("the request timed out")
	default:
		return errors.New("the connection failed")
	}
}

// retryAfter reads a Retry-After header: seconds, or an HTTP date.
func retryAfter(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(header); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if at, err := http.ParseTime(header); err == nil {
		if d := at.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// backoff is the wait before attempt+1: 500 ms doubled per attempt, up to
// maxBackoff, with a jitter of ±20 %.
func backoff(attempt int) time.Duration {
	d := 500 * time.Millisecond << min(attempt-1, 10)
	d = min(d, maxBackoff)
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // G404: jitter needs no secret randomness.
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// breaker opens after a number of failed requests in a row, refuses
// requests for the cooldown, and then lets one request through: its success
// closes the circuit, its failure opens it again. Each worker keeps its
// own.
type breaker struct {
	failures int
	cooldown time.Duration
	now      func() time.Time

	mu        sync.Mutex
	inARow    int
	openUntil time.Time
	probing   bool
}

func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inARow < b.failures {
		return true
	}
	if b.now().Before(b.openUntil) || b.probing {
		return false
	}
	b.probing = true
	return true
}

func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inARow, b.probing = 0, false
}

// release ends a probe that said nothing of the source.
func (b *breaker) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}

func (b *breaker) failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inARow++
	b.probing = false
	if b.inARow >= b.failures {
		b.openUntil = b.now().Add(b.cooldown)
	}
}
