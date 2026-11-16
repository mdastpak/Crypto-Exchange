package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync/atomic"
	"time"
)

// ErrOutcomeUnknown is a request that changes something whose answer was
// lost after the request was sent, such as a timeout: the source may have
// done it. The caller must look it up, by its own id, before it tries
// again (#384).
var ErrOutcomeUnknown = errors.New("source: the request was sent, and its outcome is unknown")

// Request is a request of Do.
type Request struct {
	Method string
	// Path is relative to the base URL.
	Path  string
	Query url.Values
	// Body is sent as is, with ContentType.
	Body        []byte
	ContentType string
	// Header is sent with this request, after the client's own.
	Header http.Header
	// Idempotent requests are retried as GetJSON retries; any other is
	// tried once after it was sent, and a lost answer is
	// ErrOutcomeUnknown.
	Idempotent bool
	// Sign, when set, signs each attempt just before it is sent, so that a
	// timestamp in the signature is fresh. It must not log what it adds.
	Sign func(*http.Request) error
}

// Do makes the request and returns the body of a 2xx answer. Any other
// answer is a *StatusError with its body, bounded, so that the caller can
// read the source's own error code. It keeps the limits, the circuit
// breaker, and the route of GetJSON. A request that is not idempotent is
// retried only when it was never sent, such as when the connection was
// refused, or when the source answered 429.
func (c *Client) Do(ctx context.Context, r Request) ([]byte, error) {
	if !c.breaker.allow() {
		return nil, fmt.Errorf("source %s: %w", c.opts.Source, ErrCircuitOpen)
	}
	target := c.base.JoinPath(r.Path)
	target.RawQuery = r.Query.Encode()
	var lastErr error
	for attempt := 1; attempt <= c.opts.MaxAttempts; attempt++ {
		if c.opts.Limiter != nil {
			if err := c.opts.Limiter.Acquire(ctx); err != nil {
				return nil, fmt.Errorf("source %s: %w", c.opts.Source, err)
			}
		}
		body, wait, err := c.do(ctx, target, r)
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
		c.breaker.release()
	default:
		c.breaker.success()
	}
	return nil, lastErr
}

// maxErrorBody bounds the body kept in a StatusError.
const maxErrorBody = 64 << 10

// do makes one attempt of r. wait is as for attempt.
func (c *Client) do(ctx context.Context, target *url.URL, r Request) (body []byte, wait time.Duration, err error) {
	var sent atomic.Bool
	traced := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { sent.Store(true) },
	})
	var reader io.Reader
	if r.Body != nil {
		reader = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(traced, r.Method, target.String(), reader)
	if err != nil {
		return nil, -1, err
	}
	for k, vs := range c.opts.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if r.ContentType != "" {
		req.Header.Set("Content-Type", r.ContentType)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	if r.Sign != nil {
		if err := r.Sign(req); err != nil {
			return nil, -1, fmt.Errorf("source %s: signing %s: %w", c.opts.Source, r.Path, err)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		err = fmt.Errorf("source %s: %s: %w", c.opts.Source, r.Path, networkError(err))
		switch {
		case r.Idempotent:
			return nil, 0, err
		case sent.Load():
			return nil, -1, errors.Join(ErrOutcomeUnknown, err)
		default:
			// Never sent: it is safe to try again.
			return nil, 0, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, err := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxBodyBytes+1))
		if err != nil {
			err = fmt.Errorf("source %s: reading %s: %w", c.opts.Source, r.Path, networkError(err))
			if r.Idempotent {
				return nil, 0, err
			}
			return nil, -1, errors.Join(ErrOutcomeUnknown, err)
		}
		if int64(len(body)) > c.opts.MaxBodyBytes {
			return nil, -1, fmt.Errorf("source %s: %s answered more than %d bytes", c.opts.Source, r.Path, c.opts.MaxBodyBytes)
		}
		return body, 0, nil
	}
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	statusErr := &StatusError{Source: c.opts.Source, Path: r.Path, Code: resp.StatusCode, Body: errBody}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		// Refused before it was done: safe to try again, as asked.
		after, ok := retryAfter(resp.Header.Get("Retry-After"), time.Now())
		if ok && after > maxRetryAfter {
			return nil, -1, fmt.Errorf("%w, and asked to wait %s", statusErr, after.Round(time.Second))
		}
		return nil, after, statusErr
	case resp.StatusCode >= 500 && r.Idempotent:
		after, _ := retryAfter(resp.Header.Get("Retry-After"), time.Now())
		if after > maxRetryAfter {
			return nil, -1, statusErr
		}
		return nil, after, statusErr
	case resp.StatusCode >= 500:
		// The source failed while it handled it: it may have done it.
		return nil, -1, errors.Join(ErrOutcomeUnknown, statusErr)
	}
	return nil, -1, statusErr
}
