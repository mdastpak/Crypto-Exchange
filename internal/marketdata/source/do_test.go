package source

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hangUp drops the connection without an answer, after the request was
// read: the server did it, and its answer is lost.
func hangUp(t *testing.T, w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	require.NoError(t, err)
	_ = conn.Close()
}

func TestDo_SendsTheBodyAndSignsEachAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		assert.Equal(t, `{"a":1}`, string(body))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "x", r.URL.Query().Get("q"))
		assert.Equal(t, string(rune('0'+n)), r.Header.Get("X-Signed"))
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, ClientOptions{})
	var signed int
	body, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "thing", Query: map[string][]string{"q": {"x"}},
		Body: []byte(`{"a":1}`), ContentType: "application/json", Idempotent: true,
		Sign: func(r *http.Request) error { signed++; r.Header.Set("X-Signed", string(rune('0'+signed))); return nil }})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true}`, string(body))
	assert.Equal(t, 2, signed, "a retry is signed again, with a fresh timestamp")
}

func TestDo_ALostAnswerOfAChangeIsAnUnknownOutcome(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		hangUp(t, w)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, ClientOptions{})
	_, err := c.Do(context.Background(), Request{Method: http.MethodPost, Path: "order"})
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	assert.Equal(t, int32(1), calls.Load(), "a change whose answer was lost is never sent again")
}

func TestDo_AFailureOfTheSourceOnAChangeIsAnUnknownOutcome(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, ClientOptions{})
	_, err := c.Do(context.Background(), Request{Method: http.MethodPost, Path: "order"})
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	var status *StatusError
	require.ErrorAs(t, err, &status)
	assert.Equal(t, http.StatusBadGateway, status.Code)
	assert.Equal(t, int32(1), calls.Load())
}

func TestDo_AChangeRefusedFor429IsTriedAgain(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, sleeps := newTestClient(t, srv.URL, ClientOptions{})
	_, err := c.Do(context.Background(), Request{Method: http.MethodPost, Path: "order"})
	require.NoError(t, err)
	assert.Equal(t, int32(2), calls.Load())
	assert.Len(t, sleeps.waits, 1)
}

func TestDo_ARefusalKeepsItsBodyOutOfTheMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":-2010,"msg":"Account has insufficient balance"}`))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv.URL, ClientOptions{})
	_, err := c.Do(context.Background(), Request{Method: http.MethodPost, Path: "order"})
	var status *StatusError
	require.ErrorAs(t, err, &status)
	assert.False(t, errors.Is(err, ErrOutcomeUnknown))
	assert.JSONEq(t, `{"code":-2010,"msg":"Account has insufficient balance"}`, string(status.Body))
	assert.NotContains(t, err.Error(), "insufficient")
}

func TestDo_AChangeThatWasNeverSentIsTriedAgain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()
	c, sleeps := newTestClient(t, addr, ClientOptions{MaxAttempts: 2})
	_, err := c.Do(context.Background(), Request{Method: http.MethodPost, Path: "order"})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrOutcomeUnknown), "a refused connection sent nothing")
	assert.Len(t, sleeps.waits, 1)
}
