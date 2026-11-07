package public

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/otp"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postFrom sends a JSON request from the client address remote.
func postFrom(t *testing.T, router *gin.Engine, remote, path string, body interface{}) (int, models.APIResponse) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remote
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var resp models.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return w.Code, resp
}

// TestVerifyOTP_RealSchema_StrangerCannotLockOutTheOwner checks #187 through
// the routes: a stranger who knows a user's email address requests codes
// for it and fails them, and the user can still sign in from elsewhere.
func TestVerifyOTP_RealSchema_StrangerCannotLockOutTheOwner(t *testing.T) {
	handler, router, sender := newOTPLimitRouter(t)
	handler.OTP = otp.NewStore(handler.RedisClient.Client, otp.Settings{AttemptsPerCode: 3, ContactAttemptsPerHour: 3})
	const victim = "victim@example.com"
	const stranger, owner = "198.51.100.66:4000", "203.0.113.7:5000"

	status, resp := postFrom(t, router, stranger, "/public/auth", map[string]string{"username": victim})
	require.Equal(t, http.StatusOK, status, resp.Message)
	sessionID := resp.Info.(map[string]interface{})["uuid"].(string)
	wrong := "000000"
	if lastSentCode(t, sender) == wrong {
		wrong = "111111"
	}
	for i := 0; i < 3; i++ {
		status, _ = postFrom(t, router, stranger, "/public/auth/"+sessionID, map[string]string{"otp": wrong})
		require.Equal(t, http.StatusUnauthorized, status)
	}
	status, _ = postFrom(t, router, stranger, "/public/auth", map[string]string{"username": victim})
	require.Equal(t, http.StatusTooManyRequests, status, "the stranger has used up its own attempts")

	status, resp = postFrom(t, router, owner, "/public/auth", map[string]string{"username": victim})
	require.Equal(t, http.StatusOK, status, "the owner can still request a code: %s", resp.Message)
	sessionID = resp.Info.(map[string]interface{})["uuid"].(string)
	status, resp = postFrom(t, router, owner, "/public/auth/"+sessionID, map[string]string{"otp": lastSentCode(t, sender)})
	assert.Equal(t, http.StatusOK, status, "and sign in: %s", resp.Message)
}
