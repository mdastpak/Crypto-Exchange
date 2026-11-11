package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/middleware"
)

// The admin listener (#308) is a second address of the same HTTP server, so
// that the admin routes can stay on a private network while the API of users
// is public. One server owns both listeners: a shutdown drains and closes
// them together, and the limits, timeouts, and TLS settings are the same.

// adminListenerPaths are the paths that the admin listener serves: the admin
// routes and the health probes, which its load balancer needs. Any other
// path answers 404 there, so that the private address is no second way into
// the API of users.
var adminListenerPaths = []string{helpers.APIPrefix + "/admin/", "/system/"}

// onAdminListener reports whether the admin listener serves the path.
func onAdminListener(path string) bool {
	for _, prefix := range adminListenerPaths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// withAdminListener returns the handler of a server that also serves the
// admin listener: a request that arrived there reaches next only for the
// paths of adminListenerPaths.
func withAdminListener(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if middleware.FromAdminListener(r) && !onAdminListener(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// listeners are the addresses that one HTTP server serves.
type listeners struct {
	public net.Listener
	// admin is nil when server.admin_listen_addr is not set.
	admin net.Listener
}

// openListeners binds the address of the server and, when adminAddr is set,
// the admin one. Binding both before either serves makes a wrong admin
// address stop the start, instead of leaving an instance without its admin
// routes.
func openListeners(addr, adminAddr string) (listeners, error) {
	public, err := net.Listen("tcp", addr)
	if err != nil {
		return listeners{}, fmt.Errorf("cannot listen on %s: %w", addr, err)
	}
	if adminAddr == "" {
		return listeners{public: public}, nil
	}
	admin, err := net.Listen("tcp", adminAddr)
	if err != nil {
		_ = public.Close()
		return listeners{}, fmt.Errorf("cannot listen on server.admin_listen_addr %s: %w", adminAddr, err)
	}
	return listeners{public: public, admin: admin}, nil
}

// attach makes srv mark the requests of the admin listener and limit them
// to its paths. Call it before serve.
func (l listeners) attach(srv *http.Server) {
	if l.admin == nil {
		return
	}
	adminAddr := l.admin.Addr().String()
	srv.Handler = withAdminListener(srv.Handler)
	srv.BaseContext = func(listener net.Listener) context.Context {
		// With TLS the server sees a wrapper of the listener, which has
		// the same address.
		if listener.Addr().String() == adminAddr {
			return middleware.OnAdminListener(context.Background())
		}
		return context.Background()
	}
}

// serve serves every listener with srv until one of them stops, and returns
// its error: http.ErrServerClosed after a shutdown, which closes them all.
// certFile and keyFile, when set, make it serve TLS.
func (l listeners) serve(srv *http.Server, certFile, keyFile string) error {
	one := func(listener net.Listener) error {
		if certFile != "" {
			return srv.ServeTLS(listener, certFile, keyFile)
		}
		return srv.Serve(listener)
	}
	if l.admin == nil {
		return one(l.public)
	}
	stopped := make(chan error, 2)
	go func() { stopped <- one(l.public) }()
	go func() { stopped <- one(l.admin) }()
	return <-stopped
}
