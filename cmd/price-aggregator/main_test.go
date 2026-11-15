package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func TestProbes(t *testing.T) {
	p := &probes{view: newViewAccess("", false)}
	h := p.handler()
	code, body := get(t, h, "/system/live")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, `{"status":"ok"}`, body)
	code, body = get(t, h, "/system/ready")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, `{"status":"ready"}`, body)
	code, body = get(t, h, "/prices")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, `{"prices":[]}`, body, "no service: no price")
	code, body = get(t, h, "/providers")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, `{"providers":[]}`, body)
	code, body = get(t, h, "/metrics")
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, "go_goroutines")

	p.draining.Store(true)
	code, body = get(t, h, "/system/ready")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, `{"status":"draining"}`, body)
}

func TestServe_StopsOnTheSignal(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	p := &probes{}
	srv := &http.Server{Addr: addr, Handler: p.handler(), ReadHeaderTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, srv, p) }()
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/system/live")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 2*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
	assert.True(t, p.draining.Load())
}

// TestView_NeedsTheToken checks the operators' view of #454: with a token,
// /prices and /providers answer 401 without it; in release mode without a
// token they are off; the probes and the metrics stay open.
func TestView_NeedsTheToken(t *testing.T) {
	request := func(h http.Handler, path, auth string) int {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	for _, release := range []bool{false, true} {
		h := (&probes{view: newViewAccess("s3cret-token", release)}).handler()
		for _, path := range []string{"/prices", "/providers"} {
			assert.Equal(t, http.StatusUnauthorized, request(h, path, ""), path)
			assert.Equal(t, http.StatusUnauthorized, request(h, path, "Bearer wrong"), path)
			assert.Equal(t, http.StatusUnauthorized, request(h, path, "s3cret-token"), "%s: a bearer token only", path)
			assert.Equal(t, http.StatusOK, request(h, path, "Bearer s3cret-token"), path)
		}
		for _, path := range []string{"/system/live", "/system/ready", "/metrics"} {
			assert.Equal(t, http.StatusOK, request(h, path, ""), path)
		}
	}
	off := (&probes{view: newViewAccess("", true)}).handler()
	assert.Equal(t, http.StatusNotFound, request(off, "/prices", ""), "off in release mode without a token")
	assert.Equal(t, http.StatusNotFound, request(off, "/providers", "Bearer anything"))
	assert.Equal(t, http.StatusOK, request(off, "/system/ready", ""))
}
