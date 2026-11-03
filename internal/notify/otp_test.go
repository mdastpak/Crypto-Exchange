package notify

import (
	"bytes"
	"context"
	"log"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewOTPSender(t *testing.T) {
	sender, err := NewOTPSender(false, false)
	require.NoError(t, err)
	assert.IsType(t, DiscardSender{}, sender, "codes are not logged unless explicitly enabled")

	sender, err = NewOTPSender(false, true)
	require.NoError(t, err)
	assert.IsType(t, DiscardSender{}, sender)

	sender, err = NewOTPSender(true, false)
	require.NoError(t, err)
	assert.IsType(t, DevLogSender{}, sender)

	_, err = NewOTPSender(true, true)
	assert.ErrorIs(t, err, ErrDevLogCodesInRelease, "development code logging must not start in release mode")
}

func TestDevLogSender_LogsOnlyTheMaskedContact(t *testing.T) {
	var buf bytes.Buffer
	sender := DevLogSender{Logger: log.New(&buf, "", 0)}

	require.NoError(t, sender.SendOTP(context.Background(), OTPMessage{
		ContactType:   "email",
		Contact:       "alice@example.com",
		MaskedContact: "ali*****@example.com",
		Code:          "123456",
	}))

	assert.Contains(t, buf.String(), "123456")
	assert.Contains(t, buf.String(), "ali*****@example.com")
	assert.NotContains(t, buf.String(), "alice@example.com", "the raw contact must never be logged")
}
