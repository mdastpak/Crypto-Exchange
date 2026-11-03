package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"encrypted-db/config"
	"encrypted-db/internal/notify"
	"encrypted-db/internal/otp"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clientIPThroughEngine sends a request with a forged X-Forwarded-For header
// from remoteAddr and returns the client IP that the engine reports.
func clientIPThroughEngine(t *testing.T, trustedProxies []string, remoteAddr string) string {
	t.Helper()
	saved := config.Config.Server.TrustedProxies
	t.Cleanup(func() { config.Config.Server.TrustedProxies = saved })
	config.Config.Server.TrustedProxies = trustedProxies

	gin.SetMode(gin.TestMode)
	engine, err := newEngine()
	require.NoError(t, err)
	var seen string
	engine.GET("/", func(c *gin.Context) { seen = c.ClientIP() })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remoteAddr
	req.Header.Set("X-Forwarded-For", "10.9.9.9")
	engine.ServeHTTP(httptest.NewRecorder(), req)
	return seen
}

func TestNewEngine_TrustsNoProxiesByDefault(t *testing.T) {
	assert.Equal(t, "203.0.113.7", clientIPThroughEngine(t, nil, "203.0.113.7:40000"),
		"without configured proxies, forwarding headers must be ignored")
}

func TestNewEngine_TrustsConfiguredProxies(t *testing.T) {
	assert.Equal(t, "10.9.9.9", clientIPThroughEngine(t, []string{"192.0.2.0/24"}, "192.0.2.10:40000"))
	assert.Equal(t, "203.0.113.7", clientIPThroughEngine(t, []string{"192.0.2.0/24"}, "203.0.113.7:40000"))
}

func TestNewEngine_RejectsInvalidProxyEntries(t *testing.T) {
	saved := config.Config.Server.TrustedProxies
	t.Cleanup(func() { config.Config.Server.TrustedProxies = saved })
	config.Config.Server.TrustedProxies = []string{"not-an-address"}

	_, err := newEngine()
	assert.Error(t, err)
}

func withOTPDeliveryConfig(t *testing.T, devLogCodes bool) {
	t.Helper()
	saved := config.Config
	t.Cleanup(func() { config.Config = saved })
	config.Config.OTP.Auth.DevLogCodes = devLogCodes
	config.Config.OTP.Auth.TTL = 60
	config.Config.RabbitMQ.Exchanges.Notifications = "notifications"
}

func TestNewOTPDelivery_DevLogCodesWritesCodesToTheLog(t *testing.T) {
	withOTPDeliveryConfig(t, true)
	email := &notify.EmailDeliverer{}

	sender, worker := newOTPDelivery(notify.Channels{Email: email}, nil)

	assert.IsType(t, notify.DevLogSender{}, sender, "development logging replaces delivery")
	assert.Nil(t, worker)
}

func TestNewOTPDelivery_WithoutChannelsRefusesSignIn(t *testing.T) {
	withOTPDeliveryConfig(t, false)

	sender, worker := newOTPDelivery(notify.Channels{}, nil)

	assert.False(t, sender.Supports("email"), "sign-in must be refused rather than claim to send a code")
	assert.False(t, sender.Supports("mobile"))
	assert.Nil(t, worker, "there is nothing to deliver")
}

func TestNewOTPDelivery_QueuesCodesForTheConfiguredChannels(t *testing.T) {
	withOTPDeliveryConfig(t, false)

	sender, worker := newOTPDelivery(notify.Channels{Email: &notify.EmailDeliverer{}}, nil)

	assert.IsType(t, &notify.QueueSender{}, sender)
	assert.True(t, sender.Supports("email"))
	assert.False(t, sender.Supports("mobile"), "mobile sign-in needs an SMS provider")
	assert.NotNil(t, worker)
}

func TestOTPTTL_MatchesTheStoreDefault(t *testing.T) {
	withOTPDeliveryConfig(t, false)
	config.Config.OTP.Auth.TTL = 0

	assert.Equal(t, otp.DefaultTTL, otpTTL(), "queued codes must expire with the store's sessions")
}
