package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserPreferences_EndToEnd checks the saved locale and theme (#286):
// the profile reads and changes them, the saved locale applies to every
// later signed-in request that names no locale of its own, and an explicit
// choice of the request still wins.
func TestUserPreferences_EndToEnd(t *testing.T) {
	env := newE2EEnv(t)
	config.Config.RateLimits.REST.RequestsPerSecond, config.Config.RateLimits.REST.Burst = 1000, 1000
	router := newTestRouter(t, env)
	token, _ := env.signedInUser(t, "prefs@example.com")

	call := func(method, path string, body interface{}, header map[string]string) (*httptest.ResponseRecorder, map[string]interface{}) {
		t.Helper()
		var payload []byte
		if body != nil {
			var err error
			payload, err = json.Marshal(body)
			require.NoError(t, err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var resp map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
		return w, resp
	}
	info := func(resp map[string]interface{}) map[string]interface{} {
		return resp["info"].(map[string]interface{})
	}

	w, resp := call(http.MethodGet, "/api/v1/user/profile", nil, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Nil(t, info(resp)["locale"], "no choice yet")
	assert.Equal(t, "system", info(resp)["theme"])
	assert.Equal(t, "en", w.Header().Get("Content-Language"))

	// Invalid values change nothing.
	w, resp = call(http.MethodPatch, "/api/v1/user/profile", map[string]string{"locale": "de", "theme": "blue"}, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	errs := info(resp)["errors"].(map[string]interface{})
	assert.Contains(t, errs, "locale")
	assert.Contains(t, errs, "theme")

	// The answer of the change is already in the saved locale.
	w, resp = call(http.MethodPatch, "/api/v1/user/profile", map[string]string{"locale": "FA", "theme": "dark"}, nil)
	require.Equal(t, http.StatusOK, w.Code, resp["message"])
	assert.Equal(t, "fa", info(resp)["locale"], "kept as the supported tag")
	assert.Equal(t, "dark", info(resp)["theme"])
	assert.Equal(t, "fa", w.Header().Get("Content-Language"))

	// Later requests use it over Accept-Language, but not over an explicit
	// choice of the request.
	w, resp = call(http.MethodGet, "/api/v1/user/profile", nil, map[string]string{"Accept-Language": "en"})
	assert.Equal(t, "fa", w.Header().Get("Content-Language"))
	assert.Equal(t, "پروفایل دریافت شد.", resp["message"])
	w, _ = call(http.MethodGet, "/api/v1/user/profile?lang=en", nil, nil)
	assert.Equal(t, "en", w.Header().Get("Content-Language"))
	w, _ = call(http.MethodGet, "/api/v1/user/profile", nil, map[string]string{"X-Locale": "en"})
	assert.Equal(t, "en", w.Header().Get("Content-Language"))

	// Clearing the choice gives the negotiation back to the request.
	w, resp = call(http.MethodPatch, "/api/v1/user/profile", map[string]string{"locale": ""}, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Nil(t, info(resp)["locale"])
	assert.Equal(t, "dark", info(resp)["theme"], "a field that is not sent keeps its value")
	w, _ = call(http.MethodGet, "/api/v1/user/profile", nil, nil)
	assert.Equal(t, "en", w.Header().Get("Content-Language"))
}
