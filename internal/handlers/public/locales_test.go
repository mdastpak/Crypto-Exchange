package public

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/i18n"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocales_ListsTheLocalesAndTheOneOfTheRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	registry := i18n.MustNew("en", "en", "fa")
	engine := gin.New()
	engine.Use(i18n.Middleware(registry))
	engine.GET("/public/locales", Locales(registry))

	get := func(set func(*http.Request)) (*httptest.ResponseRecorder, LocalesInfo, string) {
		req := httptest.NewRequest(http.MethodGet, "/public/locales", nil)
		set(req)
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		var body struct {
			Message string      `json:"message"`
			Info    LocalesInfo `json:"info"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		return w, body.Info, body.Message
	}

	w, info, message := get(func(r *http.Request) { r.Header.Set("Accept-Language", "fa-IR,fa;q=0.9") })
	assert.Equal(t, "en", info.Default)
	assert.Equal(t, "fa", info.Current.Code)
	assert.Equal(t, "rtl", info.Current.Dir)
	assert.Equal(t, "فارسی", info.Current.NativeName)
	assert.Equal(t, i18n.SourceAcceptLanguage, info.Source)
	require.Len(t, info.Locales, 2)
	assert.Equal(t, "en", info.Locales[0].Code)
	assert.Equal(t, "ltr", info.Locales[0].Dir)
	assert.Equal(t, "fa", info.Locales[1].Code)
	assert.Equal(t, "فهرست زبان\u200cها دریافت شد.", message)
	assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, "fa", w.Header().Get("Content-Language"))

	_, info, message = get(func(r *http.Request) {
		r.Header.Set("Accept-Language", "fa")
		r.AddCookie(&http.Cookie{Name: i18n.Cookie, Value: "en"})
	})
	assert.Equal(t, "en", info.Current.Code)
	assert.Equal(t, i18n.SourceCookie, info.Source)
	assert.Equal(t, "Locales retrieved successfully.", message)

	_, info, _ = get(func(*http.Request) {})
	assert.Equal(t, i18n.SourceDefault, info.Source)
}
