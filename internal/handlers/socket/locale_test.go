package socket

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// localizedGateway serves h on /ws behind the locale middleware, as the
// server does, and returns the WebSocket URL.
func localizedGateway(t *testing.T, h *WebSocketHandler) string {
	t.Helper()
	router := gin.New()
	router.Use(i18n.Middleware(i18n.MustNew("en", "en", "fa")))
	router.GET("/ws", h.ServeWSGin)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
}

// TestServe_RefusesInTheLocaleOfTheRequest checks that an upgrade refusal
// is rendered in the negotiated locale (#296), with its code unchanged.
func TestServe_RefusesInTheLocaleOfTheRequest(t *testing.T) {
	h := newTestHandler()
	h.Authenticate = fakeAuth
	url := localizedGateway(t, h)

	_, resp, err := dial(t, url+"?lang=fa", http.Header{"Authorization": {"Bearer forged"}})
	require.Error(t, err)
	defer resp.Body.Close()
	var body models.APIResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "token_invalid", body.Code)
	assert.Equal(t, "دسترسی غیرمجاز: توکن نامعتبر است.", body.Message)
	assert.Equal(t, "fa", resp.Header.Get("Content-Language"))
	assert.Contains(t, resp.Header.Values("Vary"), i18n.VaryHeaders)

	_, resp, err = dial(t, url, http.Header{"Authorization": {"Bearer disabled"}, "Accept-Language": {"en"}})
	require.Error(t, err)
	defer resp.Body.Close()
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "This account is not available. Please contact support.", body.Message)
	assert.Equal(t, "en", resp.Header.Get("Content-Language"))
}

// TestConnection_SpeaksTheSavedLocaleOfItsUser checks that the events of a
// connection use the user's saved locale (#286, #296), unless the upgrade
// chose a locale explicitly.
func TestConnection_SpeaksTheSavedLocaleOfItsUser(t *testing.T) {
	h := newTestHandler()
	h.Authenticate = func(ctx context.Context, token string) (*auth.Claims, error) {
		switch token {
		case "persian":
			return &auth.Claims{UUID: "user-1", SessionID: "session-1", Locale: "fa"}, nil
		case "broken":
			return nil, errors.New("database down")
		}
		return nil, auth.ErrInvalidToken
	}
	url := localizedGateway(t, h)

	errorMessage := func(target string) string {
		t.Helper()
		conn, _, err := dial(t, target, http.Header{"Authorization": {"Bearer persian"}, "Accept-Language": {"en"}})
		require.NoError(t, err)
		require.NoError(t, conn.WriteJSON(map[string]string{"event": "reauth", "token": "broken"}))
		reply := readEvent(t, conn, "error")
		assert.Equal(t, "unavailable", reply["code"])
		message, _ := reply["message"].(string)
		return message
	}
	assert.Equal(t, "بررسی توکن انجام نشد؛ دوباره تلاش کنید.", errorMessage(url), "the saved locale wins over Accept-Language")
	assert.Equal(t, "Could not check the token; try again", errorMessage(url+"?lang=en"), "an explicit choice wins over the saved locale")
}

// TestServe_WithoutTheLocaleMiddlewareStaysEnglish keeps the gateway in
// English where no locale was negotiated.
func TestServe_WithoutTheLocaleMiddlewareStaysEnglish(t *testing.T) {
	h := newTestHandler()
	h.Authenticate = fakeAuth
	_, resp, err := dial(t, gateway(t, h), http.Header{"Authorization": {"Bearer forged"}})
	require.Error(t, err)
	defer resp.Body.Close()
	var body models.APIResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "Unauthorized - Invalid token", body.Message)
	assert.Equal(t, "en", resp.Header.Get("Content-Language"))
}
