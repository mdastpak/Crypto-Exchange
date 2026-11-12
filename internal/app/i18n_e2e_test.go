package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRouter_AnswersInTheLanguageOfTheRequest checks the locale of the
// whole pipeline (#283): the locales route, and the refusals of the
// middleware and of an unknown path, which come before any handler.
func TestRouter_AnswersInTheLanguageOfTheRequest(t *testing.T) {
	env := newE2EEnv(t)
	config.Config.RateLimits.REST.RequestsPerSecond, config.Config.RateLimits.REST.Burst = 1000, 1000
	router := newTestRouter(t, env)

	call := func(method, path string, header map[string]string) (*httptest.ResponseRecorder, map[string]interface{}) {
		req := httptest.NewRequest(method, path, nil)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var body map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
		return w, body
	}

	w, body := call(http.MethodGet, "/api/v1/public/locales", map[string]string{"Accept-Language": "fa-IR,fa;q=0.9,en;q=0.5"})
	require.Equal(t, http.StatusOK, w.Code)
	info := body["info"].(map[string]interface{})
	assert.Equal(t, "en", info["default"])
	assert.Equal(t, "fa", info["current"].(map[string]interface{})["code"])
	assert.Equal(t, "rtl", info["current"].(map[string]interface{})["dir"])
	assert.Len(t, info["locales"], 2)

	w, body = call(http.MethodGet, "/api/v1/no-such-route", map[string]string{"X-Locale": "fa"})
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "یافت نشد.", body["message"])
	assert.Equal(t, "not_found", body["code"])
	assert.Equal(t, "fa", w.Header().Get("Content-Language"))

	// The token check refuses before the handler, in the same language.
	w, body = call(http.MethodGet, "/api/v1/user/profile?lang=fa", nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "دسترسی غیرمجاز: توکن ارسال نشده است.", body["message"])

	// An unsupported language falls back to the default, English.
	w, body = call(http.MethodGet, "/api/v1/public/locales", map[string]string{"Accept-Language": "de"})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "default", body["info"].(map[string]interface{})["source"])
	assert.Equal(t, "Locales retrieved successfully.", body["message"])

	// A preflight from an allowed origin may send X-Locale.
	config.Config.Server.CORS.AllowedOrigins = []string{"https://app.example.com"}
	router = newTestRouter(t, env)
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/public/locales", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	req.Header.Set("Access-Control-Request-Headers", "X-Locale")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Contains(t, w.Header().Get("Access-Control-Allow-Headers"), "X-Locale")
}
