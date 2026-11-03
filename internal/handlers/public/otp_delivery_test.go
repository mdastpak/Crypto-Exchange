package public

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"testing"

	"encrypted-db/internal/notify"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingOTPSender struct {
	messages []notify.OTPMessage
	err      error
	// unsupported lists the contact types the sender refuses.
	unsupported []string
}

func (r *recordingOTPSender) Supports(contactType string) bool {
	for _, t := range r.unsupported {
		if t == contactType {
			return false
		}
	}
	return true
}

func (r *recordingOTPSender) SendOTP(ctx context.Context, msg notify.OTPMessage) error {
	r.messages = append(r.messages, msg)
	return r.err
}

// captureLog redirects the standard logger for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buf
}

func TestRequestOTP_RealSchema_DeliversTheCodeWithoutLoggingIt(t *testing.T) {
	handler, _, redisClient := setupRealSchemaHandler(t)
	sender := &recordingOTPSender{}
	handler.OTPSender = sender
	router := gin.New()
	router.POST("/public/auth", handler.RequestOTP)
	logs := captureLog(t)

	const contact = "private.person@example.com"
	status, resp := postJSON(t, router, "/public/auth", map[string]string{"username": contact})
	require.Equal(t, http.StatusOK, status, resp.Message)
	sessionID := resp.Info.(map[string]interface{})["uuid"].(string)

	keys, err := redisClient.Keys(context.Background(), otpKeyPrefix+sessionID+":*").Result()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	code := strings.TrimPrefix(keys[0], otpKeyPrefix+sessionID+":")

	require.Len(t, sender.messages, 1, "the code must be handed to the sender")
	msg := sender.messages[0]
	assert.Equal(t, code, msg.Code)
	assert.Equal(t, "email", msg.ContactType)
	assert.Equal(t, contact, msg.Contact)
	assert.NotContains(t, msg.MaskedContact, "private.person")

	assert.NotContains(t, logs.String(), code, "the sign-in code must not be logged")
	assert.NotContains(t, logs.String(), contact, "the raw contact must not be logged")
}

func TestRequestOTP_RealSchema_SenderFailure(t *testing.T) {
	handler, _, _ := setupRealSchemaHandler(t)
	handler.OTPSender = &recordingOTPSender{err: errors.New("provider unavailable")}
	router := gin.New()
	router.POST("/public/auth", handler.RequestOTP)
	logs := captureLog(t)

	const contact = "failing.delivery@example.com"
	status, resp := postJSON(t, router, "/public/auth", map[string]string{"username": contact})

	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, "Failed to send OTP", resp.Message)
	assert.Contains(t, logs.String(), "provider unavailable", "the failure must be logged")
	assert.NotContains(t, logs.String(), contact, "the raw contact must not be logged, even on failure")
}

func TestRequestOTP_RealSchema_ContactTypeWithoutChannel(t *testing.T) {
	handler, _, redisClient := setupRealSchemaHandler(t)
	sender := &recordingOTPSender{unsupported: []string{"mobile"}}
	handler.OTPSender = sender
	router := gin.New()
	router.POST("/public/auth", handler.RequestOTP)

	status, resp := postJSON(t, router, "/public/auth", map[string]string{"username": "09120000077"})

	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "Sign-in by mobile is not available", resp.Message)
	assert.Empty(t, sender.messages, "no code may be sent")
	keys, err := redisClient.Keys(context.Background(), otpKeyPrefix+"*").Result()
	require.NoError(t, err)
	assert.Empty(t, keys, "no code may be issued for a contact it cannot reach")

	status, resp = postJSON(t, router, "/public/auth", map[string]string{"username": "reachable@example.com"})
	assert.Equal(t, http.StatusOK, status, resp.Message)
}

func TestRequestOTP_RealSchema_UsernameIsRejected(t *testing.T) {
	handler, _, redisClient := setupRealSchemaHandler(t)
	sender := &recordingOTPSender{}
	handler.OTPSender = sender
	router := gin.New()
	router.POST("/public/auth", handler.RequestOTP)

	status, resp := postJSON(t, router, "/public/auth", map[string]string{"username": "alice_2026"})

	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "Invalid contact information", resp.Message)
	assert.Empty(t, sender.messages)
	keys, err := redisClient.Keys(context.Background(), otpKeyPrefix+"*").Result()
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func TestRequestOTP_RealSchema_DefaultSenderLogsNothing(t *testing.T) {
	handler, _, _ := setupRealSchemaHandler(t)
	require.IsType(t, notify.DiscardSender{}, handler.OTPSender, "codes are discarded unless the server configures a sender")
	router := gin.New()
	router.POST("/public/auth", handler.RequestOTP)
	logs := captureLog(t)

	const contact = "quiet.default@example.com"
	status, _ := postJSON(t, router, "/public/auth", map[string]string{"username": contact})

	assert.Equal(t, http.StatusOK, status)
	assert.NotContains(t, logs.String(), contact)
	assert.NotContains(t, logs.String(), "sign-in code")
}
