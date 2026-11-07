package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowRoute is a test route whose requests stay in flight until the test
// releases them.
const slowRoute = "/test/slow"

// testDrain is the drain of the instances in these tests: long enough to
// see the readiness probe fail while the instance still serves.
const testDrain = 2 * time.Second

// runningInstance is an instance of the server serving on a local port, as
// main serves it, with the routes of main and slowRoute.
type runningInstance struct {
	addr    string
	started chan struct{}
	release func()
	stopped chan error
}

// startInstance serves the end-to-end environment with serve until ctx is
// done.
func startInstance(t *testing.T, ctx context.Context, env *e2eEnv) *runningInstance {
	t.Helper()
	router, err := newEngine()
	require.NoError(t, err)
	routeForTest(t, router, env.handlers)
	released := make(chan struct{})
	var once sync.Once
	inst := &runningInstance{
		started: make(chan struct{}, 1),
		release: func() { once.Do(func() { close(released) }) },
		stopped: make(chan error, 1),
	}
	router.GET(slowRoute, func(c *gin.Context) {
		inst.started <- struct{}{}
		<-released
		c.String(http.StatusOK, "done")
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	inst.addr = listener.Addr().String()
	srv := &http.Server{Handler: router}
	go func() {
		inst.stopped <- serve(ctx, srv, func() error { return srv.Serve(listener) }, env.handlers, nil, 10*time.Second, testDrain)
	}()
	// Stops the instance when the test fails before it does.
	t.Cleanup(func() {
		inst.release()
		_ = srv.Close()
	})
	return inst
}

// newConnectionClient opens a new connection for every request, as a new
// client does.
var newConnectionClient = &http.Client{
	Transport: &http.Transport{DisableKeepAlives: true},
	Timeout:   5 * time.Second,
}

// probe returns the status code and the status field of a health probe.
func (inst *runningInstance) probe(path string) (int, string) {
	resp, err := newConnectionClient.Get("http://" + inst.addr + path)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.Status
}

// refusesConnections reports whether the instance no longer accepts TCP
// connections.
func (inst *runningInstance) refusesConnections() bool {
	conn, err := net.DialTimeout("tcp", inst.addr, time.Second)
	if err != nil {
		return true
	}
	conn.Close()
	return false
}

type slowResponse struct {
	status int
	body   string
	err    error
}

// checkGracefulStop runs an instance, starts a request that stays in flight,
// and calls stop, which must make ctx done. It checks the stop as a load
// balancer and the clients see it: the readiness probe fails while the
// instance keeps serving, then new connections are refused, and the request
// in flight still completes.
func checkGracefulStop(t *testing.T, ctx context.Context, stop func()) {
	env := newE2EEnv(t)
	inst := startInstance(t, ctx, env)
	require.Eventually(t, func() bool {
		status, _ := inst.probe("/system/ready")
		return status == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond, "the instance starts ready")

	inFlight := make(chan slowResponse, 1)
	go func() {
		resp, err := newConnectionClient.Get("http://" + inst.addr + slowRoute)
		if err != nil {
			inFlight <- slowResponse{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		inFlight <- slowResponse{status: resp.StatusCode, body: string(body), err: err}
	}()
	select {
	case <-inst.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow request did not reach the handler")
	}

	stop()

	require.Eventually(t, func() bool {
		status, body := inst.probe("/system/ready")
		return status == http.StatusServiceUnavailable && body == "draining"
	}, testDrain/2, 10*time.Millisecond, "the readiness probe fails while the instance drains")
	status, _ := inst.probe("/system/live")
	assert.Equal(t, http.StatusOK, status, "the instance still serves while it drains")

	require.Eventually(t, inst.refusesConnections, testDrain+5*time.Second, 20*time.Millisecond,
		"the instance refuses new connections after the drain")
	select {
	case err := <-inst.stopped:
		t.Fatalf("serve returned while a request was in flight: %v", err)
	case res := <-inFlight:
		t.Fatalf("the request in flight ended before it was released: %+v", res)
	default:
	}

	inst.release()
	select {
	case res := <-inFlight:
		require.NoError(t, res.err, "the request in flight completes")
		assert.Equal(t, http.StatusOK, res.status)
		assert.Equal(t, "done", res.body)
	case <-time.After(5 * time.Second):
		t.Fatal("the request in flight did not complete")
	}
	select {
	case err := <-inst.stopped:
		assert.NoError(t, err, "the instance stops cleanly")
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return")
	}
	select {
	case <-env.handlers.Socket.ShutdownCh:
	default:
		t.Error("the WebSocket clients and the event consumer are stopped")
	}
}

// TestServe_StopsGracefullyWhenCancelled stops an instance through its
// context; TestServe_StopsGracefullyOnSIGTERM sends the signal itself where
// the platform can.
func TestServe_StopsGracefullyWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checkGracefulStop(t, ctx, cancel)
}

// TestServe_ShutsDownWhenTheServerFails checks that a server that cannot
// listen stops the instance in order and returns its error, where main used
// to exit from the server goroutine, skipping every close.
func TestServe_ShutsDownWhenTheServerFails(t *testing.T) {
	env := newE2EEnv(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	srv := &http.Server{Addr: occupied.Addr().String(), Handler: http.NotFoundHandler()}

	start := time.Now()
	// The drain would hold the test for a minute if serve did not skip it.
	err = serve(context.Background(), srv, srv.ListenAndServe, env.handlers, nil, 10*time.Second, time.Minute)
	var listenErr *net.OpError
	require.ErrorAs(t, err, &listenErr, "serve returns the listener's error")
	assert.ErrorContains(t, err, "the HTTP server stopped")
	assert.Less(t, time.Since(start), 30*time.Second, "a server that failed skips the drain")

	select {
	case <-env.handlers.Socket.ShutdownCh:
	default:
		t.Error("the WebSocket clients and the event consumer are stopped")
	}
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/system/ready", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "the instance reports that it is not ready")
}
