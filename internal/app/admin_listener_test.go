package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/middleware"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListeners_TheAdminListenerServesOnlyItsPaths runs one server on two
// listeners (#308) and checks which requests each one lets through, and
// that only the admin one marks them.
func TestListeners_TheAdminListenerServesOnlyItsPaths(t *testing.T) {
	bound, err := openListeners("127.0.0.1:0", "127.0.0.1:0")
	require.NoError(t, err)
	require.NotNil(t, bound.admin)

	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, "admin=%t", middleware.FromAdminListener(r))
		}),
	}
	bound.attach(srv)
	stopped := make(chan error, 1)
	go func() { stopped <- bound.serve(srv, "", "") }()

	get := func(addr, path string) (int, string) {
		resp, err := http.Get("http://" + addr + path)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(body)
	}
	public, admin := bound.public.Addr().String(), bound.admin.Addr().String()
	adminPath := helpers.APIPrefix + "/admin/sessions"
	userPath := helpers.APIPrefix + "/currencies"

	status, body := get(public, userPath)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "admin=false", body)
	// The public listener passes the admin paths on unmarked: the access
	// middleware of the routes answers them.
	status, body = get(public, adminPath)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "admin=false", body)

	status, body = get(admin, adminPath)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "admin=true", body)
	status, _ = get(admin, "/system/ready")
	assert.Equal(t, http.StatusOK, status)
	status, _ = get(admin, userPath)
	assert.Equal(t, http.StatusNotFound, status, "the admin listener is no second way into the API of users")
	status, _ = get(admin, helpers.APIPrefix+"/administrators")
	assert.Equal(t, http.StatusNotFound, status, "a path that only starts like the admin prefix")

	// One shutdown closes both listeners.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, srv.Shutdown(ctx))
	assert.ErrorIs(t, <-stopped, http.ErrServerClosed)
	_, err = http.Get("http://" + admin + adminPath)
	assert.Error(t, err)
	_, err = http.Get("http://" + public + userPath)
	assert.Error(t, err)
}

func TestOpenListeners_AnAdminAddressInUseStopsTheStart(t *testing.T) {
	taken, err := openListeners("127.0.0.1:0", "")
	require.NoError(t, err)
	defer func() { _ = taken.public.Close() }()
	assert.Nil(t, taken.admin)

	_, err = openListeners("127.0.0.1:0", taken.public.Addr().String())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.admin_listen_addr")
	assert.False(t, errors.Is(err, http.ErrServerClosed))
}

// TestAdminRoutes_AllowedNetworks checks that admin.allowed_cidrs covers the
// sign-in as well as the routes behind a token, and leaves the API of users
// alone. The requests of the tests come from 192.0.2.1.
func TestAdminRoutes_AllowedNetworks(t *testing.T) {
	env := newE2EEnv(t, func(c *config.Configuration) { c.Admin.AllowedCIDRs = []string{"10.0.0.0/8"} })

	status, resp := env.post(t, helpers.APIPrefix+"/admin/auth", map[string]string{"email": "admin@example.com"}, "")
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, middleware.CodeAdminNetworkDenied, resp.Code)

	status, resp = env.post(t, helpers.APIPrefix+"/admin/step-up", map[string]string{"totp_code": "000000"}, "")
	assert.Equal(t, http.StatusForbidden, status, "refused before the token is read")
	assert.Equal(t, middleware.CodeAdminNetworkDenied, resp.Code)

	status, _ = env.post(t, helpers.APIPrefix+"/auth", map[string]string{}, "")
	assert.NotEqual(t, http.StatusForbidden, status, "the routes of users are not limited")
}

func TestAdminRoutes_AllowedNetworksLetTheirClientsIn(t *testing.T) {
	env := newE2EEnv(t, func(c *config.Configuration) { c.Admin.AllowedCIDRs = []string{"192.0.2.0/24"} })

	status, resp := env.post(t, helpers.APIPrefix+"/admin/step-up", map[string]string{"totp_code": "000000"}, "")
	assert.Equal(t, http.StatusUnauthorized, status, "past the network check, the token is missing")
	assert.NotEqual(t, middleware.CodeAdminNetworkDenied, resp.Code)
}

// TestAdminRoutes_HiddenOnThePublicListener checks that with
// server.admin_listen_addr the public listener answers the admin routes as
// routes that do not exist.
func TestAdminRoutes_HiddenOnThePublicListener(t *testing.T) {
	env := newE2EEnv(t, func(c *config.Configuration) { c.Server.AdminListenAddr = "127.0.0.1:8444" })

	for _, path := range []string{"/admin/auth", "/admin/auth/verify", "/admin/step-up", "/admin/logout"} {
		status, _ := env.post(t, helpers.APIPrefix+path, map[string]string{}, "")
		assert.Equal(t, http.StatusNotFound, status, path)
	}
}
