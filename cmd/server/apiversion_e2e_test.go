package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/helpers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unversionedRoutes are the routes that are not part of the API, and so
// have no path under helpers.APIPrefix.
func unversioned(path string) bool {
	return path == "/" || strings.HasPrefix(path, "/swagger/") || strings.HasPrefix(path, "/system/")
}

// TestAPI_EveryRouteIsVersioned checks that each route of the API is
// registered under /api/v1, and that each unversioned one is its alias.
func TestAPI_EveryRouteIsVersioned(t *testing.T) {
	env := newE2EEnv(t)
	versioned, aliases := map[string]bool{}, map[string]bool{}
	for _, route := range newTestRouter(t, env).Routes() {
		key := route.Method + " " + strings.TrimPrefix(route.Path, helpers.APIPrefix)
		switch {
		case strings.HasPrefix(route.Path, helpers.APIPrefix+"/"):
			versioned[key] = true
		case !unversioned(route.Path):
			aliases[key] = true
		}
	}
	assert.NotEmpty(t, versioned)
	assert.Equal(t, versioned, aliases, "every route has a versioned path and an unversioned alias, and nothing else")
}

// TestAPI_AliasesAnswerTheSameAndPointToTheSuccessor compares a route with
// its deprecated alias.
func TestAPI_AliasesAnswerTheSameAndPointToTheSuccessor(t *testing.T) {
	env := newE2EEnv(t)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}

	current, alias := get("/api/v1/public/currencies"), get("/public/currencies")
	require.Equal(t, http.StatusOK, current.Code)
	assert.Equal(t, current.Code, alias.Code)
	assert.Equal(t, current.Body.String(), alias.Body.String())
	assert.Empty(t, current.Header().Get("Deprecation"), "the versioned path is not deprecated")
	assert.Equal(t, "@1793923200", alias.Header().Get("Deprecation"), "deprecated since 2026-11-06")
	assert.Equal(t, `</api/v1/public/currencies>; rel="successor-version"`, alias.Header().Get("Link"))

	for _, path := range []string{"/system/live", "/system/ready"} {
		assert.Empty(t, get(path).Header().Get("Deprecation"), "%s is not part of the API", path)
	}
}

// TestAPI_TheRefreshCookieFollowsTheVersion signs in through both paths: the
// refresh token cookie is scoped to the auth routes of the path used.
func TestAPI_TheRefreshCookieFollowsTheVersion(t *testing.T) {
	env := newE2EEnv(t)
	for prefix, wantPath := range map[string]string{
		helpers.APIPrefix: "/api/v1/public/auth",
		"":                "/public/auth",
	} {
		contact := "cookie" + strings.ReplaceAll(prefix, "/", "-") + "@example.com"
		status, resp := env.post(t, prefix+"/public/auth", map[string]string{"username": contact}, "")
		require.Equal(t, http.StatusOK, status, resp.Message)
		w := env.send(t, http.MethodPost, prefix+"/public/auth/"+resp.Info["uuid"].(string),
			map[string]string{"otp": env.sender.last(contact)}, "", "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		cookie := refreshTokenCookie(w)
		require.NotNil(t, cookie)
		assert.Equal(t, wantPath, cookie.Path, "prefix %q", prefix)

		// Logout through either path ends the cookies of both.
		w = env.send(t, http.MethodPost, prefix+"/public/auth/logout", nil, accessTokenOf(t, w), cookie.Value)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		cleared := map[string]bool{}
		for _, c := range w.Result().Cookies() {
			if c.Name == cookie.Name && c.MaxAge < 0 {
				cleared[c.Path] = true
			}
		}
		assert.Equal(t, map[string]bool{"/api/v1/public/auth": true, "/public/auth": true}, cleared)
	}
}

// TestAPI_AliasesShareTheRateLimits sends sign-in code requests through both
// paths from one address. They count against one budget, so the alias does
// not double the limit.
func TestAPI_AliasesShareTheRateLimits(t *testing.T) {
	env := newE2EEnv(t)
	config.Config.RateLimits.Auth.OTPRequestsPerHour = 2
	router := newTestRouter(t, env)
	send := func(path string) int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.20:40000"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w.Code
	}

	assert.NotEqual(t, http.StatusTooManyRequests, send("/api/v1/public/auth"))
	assert.NotEqual(t, http.StatusTooManyRequests, send("/public/auth"))
	assert.Equal(t, http.StatusTooManyRequests, send("/api/v1/public/auth"), "the third request in the hour is refused")
	assert.Equal(t, http.StatusTooManyRequests, send("/public/auth"))
}
