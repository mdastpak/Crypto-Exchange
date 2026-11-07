package public

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/contact"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/otp"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// persianDigits writes the ASCII digits of s as Persian digits.
func persianDigits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return '۰' + (r - '0')
		}
		return r
	}, s)
}

// TestVerifyOTP_RealSchema_EveryFormOfAMobileReachesOneAccount checks #197:
// a number in Persian digits, with +98, or with separators, is the number
// that user_contacts stores, and a code in Persian digits is the code.
func TestVerifyOTP_RealSchema_EveryFormOfAMobileReachesOneAccount(t *testing.T) {
	handler, router, sender := newOTPLimitRouter(t)
	handler.OTP = otp.NewStore(handler.RedisClient.Client, otp.Settings{ContactRequestsPerHour: 10})
	config.Config.JWT.Sessions.MaxPerUser = 10 // every form signs in to one account
	db := handler.PostgresDB.DB
	for _, typed := range []string{"09121112233", "۰۹۱۲۱۱۱۲۲۳۳", "+98 912 111 2233", "00989121112233", "0912-111-2233", "٠٩١٢١١١٢٢٣٣"} {
		sessionID, code := requestCode(t, router, sender, typed)
		status, resp := postJSON(t, router, "/public/auth/"+sessionID, map[string]string{"otp": " " + persianDigits(code) + " "})
		require.Equal(t, http.StatusOK, status, "%q: %s", typed, resp.Message)
	}
	var users, contacts int
	require.NoError(t, db.QueryRow(`SELECT count(*), count(DISTINCT user_hk) FROM user_contacts WHERE type = 'mobile' AND value = '09121112233'`).Scan(&contacts, &users))
	assert.Equal(t, 1, contacts, "the number is stored once, in its national form")
	assert.Equal(t, 1, users, "every form reached the same account")
}

func TestRequestOTP_RealSchema_SendsToTheNormalizedContact(t *testing.T) {
	_, router, sender := newOTPLimitRouter(t)
	status, resp := postJSON(t, router, "/public/auth", map[string]string{"username": "+۹۸۹۱۲۳۴۵۶۷۸۹"})
	require.Equal(t, http.StatusOK, status, resp.Message)
	require.Len(t, sender.messages, 1)
	assert.Equal(t, "09123456789", sender.messages[0].Contact)
	assert.Equal(t, "091*****789", resp.Info.(map[string]interface{})["username"])

	status, _ = postJSON(t, router, "/public/auth", map[string]string{"username": "  alice@example.com\t"})
	require.Equal(t, http.StatusOK, status, "surrounding spaces are not part of an email address")
	assert.Equal(t, "alice@example.com", sender.messages[1].Contact)
}

// TestVerifyOTP_RealSchema_MalformedCodesSpendNoAttempt checks that a code
// that cannot be right, such as one with a letter or of the wrong length,
// is refused without moving the user toward the lockout.
func TestVerifyOTP_RealSchema_MalformedCodesSpendNoAttempt(t *testing.T) {
	handler, router, sender := newOTPLimitRouter(t)
	handler.OTP = otp.NewStore(handler.RedisClient.Client, otp.Settings{AttemptsPerCode: 2, ContactAttemptsPerHour: 2})
	sessionID, code := requestCode(t, router, sender, "typo@example.com")

	for _, malformed := range []string{"12345", "1234567", "12a456", "۱۲۳ ۴۵۶", "one-two"} {
		status, resp := postJSON(t, router, "/public/auth/"+sessionID, map[string]string{"otp": malformed})
		assert.Equal(t, http.StatusUnauthorized, status, "%q", malformed)
		assert.Equal(t, "code_invalid", resp.Code)
	}

	status, resp := postJSON(t, router, "/public/auth/"+sessionID, map[string]string{"otp": code})
	assert.Equal(t, http.StatusOK, status, "the right code still signs in: %s", resp.Message)
}

func TestIdentifyInputType_AfterNormalization(t *testing.T) {
	kind, masked := IdentifyInputType(contact.Normalize("۰۹۱۲۳۴۵۶۷۸۹"))
	assert.Equal(t, "mobile", kind)
	assert.Equal(t, "091*****789", masked)
}
