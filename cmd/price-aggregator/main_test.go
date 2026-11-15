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
	p := &probes{}
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
