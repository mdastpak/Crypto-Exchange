package public

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"testing"

	"encrypted-db/internal/notify"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingOTPSender struct {
	messages []notify.OTPMessage
	err      error
}

func (r *recordingOTPSender) SendOTP(ctx context.Context, msg notify.OTPMessage) error {
	r.messages = append(r.messages, msg)
	return r.err
}

// lastSentCode returns the code of the last message handed to sender. Tests
// read codes there because Redis only holds their hashes.
func lastSentCode(t *testing.T, sender *recordingOTPSender) string {
	t.Helper()
	require.NotEmpty(t, sender.messages, "no code was sent")
	return sender.messages[len(sender.messages)-1].Code
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

	require.Len(t, sender.messages, 1, "the code must be handed to the sender")
	msg := sender.messages[0]
	code := msg.Code
	assert.Len(t, code, 6)
	assert.Equal(t, "email", msg.ContactType)
	assert.Equal(t, contact, msg.Contact)
	assert.NotContains(t, msg.MaskedContact, "private.person")

	assert.NotContains(t, logs.String(), code, "the sign-in code must not be logged")

	keys, err := redisClient.Keys(context.Background(), "*").Result()
	require.NoError(t, err)
	assert.Contains(t, keys, "otp:session:"+sessionID)
	for _, key := range keys {
		assert.NotContains(t, key, code, "Redis keys must not contain the code")
	}
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
