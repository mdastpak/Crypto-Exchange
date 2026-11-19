// Package redirecttest checks that a client sends its credential to no
// host but the one it was configured with.
package redirecttest

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Trap is a server that redirects every request to another server, on
// another port and over plain http, which records whether it was reached.
type Trap struct {
	// Origin is the server the client is configured with.
	Origin *httptest.Server
	// URL is the base URL of Origin.
	URL     string
	reached atomic.Bool
}

// New starts a trap whose origin speaks plain http, or TLS with tlsOrigin.
// Both servers close when the test ends.
func New(t *testing.T, tlsOrigin bool) *Trap {
	t.Helper()
	trap := &Trap{}
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		trap.reached.Store(true)
	}))
	t.Cleanup(target.Close)
	// 307 keeps the method and the body, so a POST is sent again as it was.
	redirect := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The path is the target's own, so that nothing of the request
		// decides where the client goes.
		w.Header().Set("Location", target.URL+"/trap")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	if tlsOrigin {
		trap.Origin = httptest.NewTLSServer(redirect)
	} else {
		trap.Origin = httptest.NewServer(redirect)
	}
	t.Cleanup(trap.Origin.Close)
	trap.URL = trap.Origin.URL
	return trap
}

// Reached reports whether any request, with whatever it carried, arrived
// at the target of the redirect.
func (trap *Trap) Reached() bool { return trap.reached.Load() }
