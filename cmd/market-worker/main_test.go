package main

import (
	"context"
	"errors"
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

func TestProbes_ReadyWhilePostgresAnswers(t *testing.T) {
	var down error
	p := &probes{ping: func(context.Context) error { return down }}
	h := p.handler()

	code, body := get(t, h, "/system/ready")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, `{"status":"ready"}`, body)

	down = errors.New("connection refused")
	code, body = get(t, h, "/system/ready")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, `{"status":"postgres_unavailable"}`, body)

	code, _ = get(t, h, "/system/live")
	assert.Equal(t, http.StatusOK, code, "liveness does not depend on PostgreSQL")
}

func TestProbes_NotReadyWhileDraining(t *testing.T) {
	p := &probes{ping: func(context.Context) error { return nil }}
	p.draining.Store(true)
	code, body := get(t, p.handler(), "/system/ready")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, `{"status":"draining"}`, body)
}

func TestServe_StopsOnTheSignal(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	p := &probes{ping: func(context.Context) error { return nil }}
	srv := &http.Server{Addr: addr, Handler: p.handler(), ReadHeaderTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, srv, p) }()

	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/system/live")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not stop")
	}
	assert.True(t, p.draining.Load())
}

func TestServe_ReportsAFailedListen(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	p := &probes{ping: func(context.Context) error { return nil }}
	srv := &http.Server{Addr: listener.Addr().String(), Handler: p.handler(), ReadHeaderTimeout: time.Second}
	err = serve(context.Background(), srv, p)
	assert.ErrorContains(t, err, "the probe server stopped")
}

func TestApplicationName_NamesTheHost(t *testing.T) {
	assert.True(t, strings.HasPrefix(applicationName(), "crypto-exchange-market-worker/"))
}

func TestServe_StopsTheJobsAtShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	stopped := false
	p := &probes{ping: func(context.Context) error { return nil },
		stopJobs: func(ctx context.Context) error {
			_, ok := ctx.Deadline()
			assert.True(t, ok, "the jobs stop within the shutdown deadline")
			stopped = true
			return errors.New("runs cancelled at shutdown")
		}}
	srv := &http.Server{Addr: addr, Handler: p.handler(), ReadHeaderTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = serve(ctx, srv, p)
	assert.True(t, stopped)
	assert.ErrorContains(t, err, "runs cancelled at shutdown")
}
