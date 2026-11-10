package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/helpers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probes are the routes that are not part of the API: the health probes
// that orchestrators and load balancers call.
var probes = map[string]bool{"/system/live": true, "/system/ready": true}

// unversioned reports a route that is not part of the API, and so has no path
// under helpers.APIPrefix: a probe, or Swagger where it is enabled.
func unversioned(path string) bool {
	return probes[path] || strings.HasPrefix(path, "/swagger/")
}

// TestAPI_EveryRouteIsVersioned checks that every route is under /api/v1,
// besides the probes and Swagger, so that no route is reachable at a second
// path that the gates, limits, and document could miss.
func TestAPI_EveryRouteIsVersioned(t *testing.T) {
	env := newE2EEnv(t)
	versioned := 0
	for _, route := range newTestRouter(t, env).Routes() {
		if strings.HasPrefix(route.Path, helpers.APIPrefix+"/") {
			versioned++
			continue
		}
		assert.True(t, unversioned(route.Path), "%s %s is outside %s", route.Method, route.Path, helpers.APIPrefix)
	}
	assert.NotZero(t, versioned)
}

// TestAPI_TheRefreshCookieIsScopedToTheAuthRoutes signs in and out: the
// refresh token cookie is sent only to the auth routes, and logout clears it.
func TestAPI_TheRefreshCookieIsScopedToTheAuthRoutes(t *testing.T) {
	env := newE2EEnv(t)
	contact := "cookie@example.com"
	status, resp := env.post(t, "/api/v1/public/auth", map[string]string{"username": contact}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	w := env.send(t, http.MethodPost, "/api/v1/public/auth/"+resp.Info["uuid"].(string),
		map[string]string{"otp": env.sender.last(contact)}, "", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	cookie := refreshTokenCookie(w)
	require.NotNil(t, cookie)
	assert.Equal(t, "/api/v1/public/auth", cookie.Path)

	w = env.send(t, http.MethodPost, "/api/v1/public/auth/logout", nil, accessTokenOf(t, w), cookie.Value)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var cleared []string
	for _, c := range w.Result().Cookies() {
		if c.Name == cookie.Name && c.MaxAge < 0 {
			cleared = append(cleared, c.Path)
		}
	}
	assert.Equal(t, []string{"/api/v1/public/auth"}, cleared)
}
