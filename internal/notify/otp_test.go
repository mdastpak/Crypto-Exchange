package notify

import (
	"bytes"
	"context"
	"log"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckDevLogCodes(t *testing.T) {
	assert.NoError(t, CheckDevLogCodes(false, false))
	assert.NoError(t, CheckDevLogCodes(false, true))
	assert.NoError(t, CheckDevLogCodes(true, false))
	assert.ErrorIs(t, CheckDevLogCodes(true, true), ErrDevLogCodesInRelease,
		"development code logging must not start in release mode")
}

func TestSenders_SupportOnlyEmailAndMobile(t *testing.T) {
	for _, sender := range []OTPSender{DiscardSender{}, DevLogSender{}} {
		assert.True(t, sender.Supports(ContactEmail))
		assert.True(t, sender.Supports(ContactMobile))
		assert.False(t, sender.Supports("username"))
		assert.False(t, sender.Supports(""))
	}
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
