package public

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertBareError checks that a response is the standard envelope with the
// expected status and message, and the code of the status, and nothing else:
// no info, and none of the internal details in leaks.
func assertBareError(t *testing.T, w *httptest.ResponseRecorder, status int, message string, leaks ...string) {
	t.Helper()
	assert.Equal(t, status, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	assert.Equal(t, map[string]interface{}{"status": float64(status), "message": message, "code": helpers.StatusCode(status)}, body)
	for _, leak := range leaks {
		assert.NotContains(t, strings.ToLower(w.Body.String()), strings.ToLower(leak))
	}
}

func sendJSON(router *gin.Engine, path string, body interface{}) *httptest.ResponseRecorder {
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestOTPEndpoints_RedisFailuresRevealNoDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	captureLog(t)
	// Nothing listens on port 1, so every Redis command fails.
	unreachable := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { unreachable.Close() })
	handler := NewHandler(&models.InfraServices{Redis: &db.RedisService{Client: unreachable}})
	router := gin.New()
	router.POST("/public/auth", handler.RequestOTP)
	router.POST("/public/auth/:uuid", handler.VerifyOTP)

	w := sendJSON(router, "/public/auth", map[string]string{"username": "alice@example.com"})
	assertBareError(t, w, http.StatusInternalServerError, "Failed to store OTP", "dial", "127.0.0.1", "refused", "redis")

	w = sendJSON(router, "/public/auth/5b0c8f0e-58a6-4c3b-9d8e-2f7a1c3e9b10", map[string]string{"otp": "123456"})
	assertBareError(t, w, http.StatusInternalServerError, "Failed to verify OTP", "dial", "127.0.0.1", "refused", "redis")
}

func TestVerifyOTP_RealSchema_DatabaseFailuresRevealNoDetails(t *testing.T) {
	handler, conn, _ := setupRealSchemaHandler(t)
	sender := &recordingOTPSender{}
	handler.OTPSender = sender
	router := gin.New()
	router.POST("/public/auth", handler.RequestOTP)
	router.POST("/public/auth/:uuid", handler.VerifyOTP)
	captureLog(t)

	w := sendJSON(router, "/public/auth", map[string]string{"username": "db.down@example.com"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp models.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	sessionID := resp.Info.(map[string]interface{})["uuid"].(string)

	require.NoError(t, conn.Close(), "simulate a database outage")
	w = sendJSON(router, "/public/auth/"+sessionID, map[string]string{"otp": lastSentCode(t, sender)})

	assertBareError(t, w, http.StatusInternalServerError, "Failed to create user", "sql", "closed", "database")
}

// TestUnauthenticatedPathsUseNoPasswordHashing keeps deliberately slow key
// derivation functions out of the code that serves unauthenticated requests.
// One call per request would let anyone exhaust the CPU, as the removed PBKDF2
// contact encryption did (#16). It checks the imports of this module's
// packages behind those requests; libraries are out of scope, since pgx uses
// PBKDF2 once per database connection for SCRAM authentication. A KDF needed
// for an authenticated feature belongs in a package outside this tree.
//
// The account password (#307) is checked on a route without a token, the
// second step of a sign-in. Its hash is internal/passwordhash, which the
// server gives to internal/userauth through an interface, so this tree
// still imports none. It runs only for a challenge that a correct code
// issued, after an attempt was counted against the lock of the password,
// and a bounded number at once: internal/app
// TestUserPassword_NothingIsHashedWithoutAChallenge holds that.
func TestUnauthenticatedPathsUseNoPasswordHashing(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go tool is not available")
	}
	packages := []string{
		"github.com/mdastpak/crypto-exchange/internal/handlers/public",
		"github.com/mdastpak/crypto-exchange/internal/middleware",
	}
	args := append([]string{"list", "-deps", "-f", "{{.ImportPath}} {{join .Imports \" \"}}"}, packages...)
	out, err := exec.Command(goTool, args...).CombinedOutput()
	require.NoError(t, err, string(out))

	checked := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "github.com/mdastpak/crypto-exchange/") {
			continue
		}
		checked++
		for _, imported := range fields[1:] {
			for _, kdf := range []string{"pbkdf2", "bcrypt", "scrypt", "argon2"} {
				assert.NotContains(t, imported, kdf,
					"%s imports the password-hashing KDF %s, which must not run on unauthenticated requests", fields[0], imported)
			}
		}
	}
	assert.GreaterOrEqual(t, checked, 5, "the handler, its OTP, notification and auth packages must be checked")
}
