package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/otp"

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

	sender, worker := newOTPDelivery(notify.Channels{Email: email}, nil, nil)

	assert.IsType(t, notify.DevLogSender{}, sender, "development logging replaces delivery")
	assert.Nil(t, worker)
}

func TestNewOTPDelivery_WithoutChannelsRefusesSignIn(t *testing.T) {
	withOTPDeliveryConfig(t, false)

	sender, worker := newOTPDelivery(notify.Channels{}, nil, nil)

	assert.False(t, sender.Supports("email"), "sign-in must be refused rather than claim to send a code")
	assert.False(t, sender.Supports("mobile"))
	assert.Nil(t, worker, "there is nothing to deliver")
}

func TestNewOTPDelivery_QueuesCodesForTheConfiguredChannels(t *testing.T) {
	withOTPDeliveryConfig(t, false)

	sender, worker := newOTPDelivery(notify.Channels{Email: &notify.EmailDeliverer{}}, nil, nil)

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

// writeKeyPair writes a new RSA key pair to dir and returns the paths.
func writeKeyPair(t *testing.T, dir, name string) config.JWTKeyPaths {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	privatePath := filepath.Join(dir, name+"_private.pem")
	publicPath := filepath.Join(dir, name+"_public.pem")
	require.NoError(t, os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600))
	require.NoError(t, os.WriteFile(publicPath, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&key.PublicKey),
	}), 0o600))
	return config.JWTKeyPaths{PrivateKey: config.KeyPath(privatePath), PublicKey: config.KeyPath(publicPath)}
}

func withJWTKeys(t *testing.T, user, admin config.JWTKeyPaths) {
	t.Helper()
	saved := config.Config.JWT
	t.Cleanup(func() { config.Config.JWT = saved })
	config.Config.JWT.SSL.User = user
	config.Config.JWT.SSL.Admin = admin
	config.Config.JWT.Issuer = "test-issuer"
	config.Config.JWT.AccessToken.Expiration = 15
}

func TestInitAuthKeys_LoadsSeparateKeysPerRole(t *testing.T) {
	dir := t.TempDir()
	user := writeKeyPair(t, dir, "user")
	retired := writeKeyPair(t, dir, "retired")
	user.RetiredPublicKeys = []config.KeyPath{retired.PublicKey}
	withJWTKeys(t, user, writeKeyPair(t, dir, "admin"))

	require.NoError(t, initAuthKeys())

	assert.True(t, auth.KeysLoaded(auth.RoleUser))
	assert.True(t, auth.KeysLoaded(auth.RoleAdmin))
	userToken, err := auth.GenerateAccessToken("user-1", auth.RoleUser, "")
	require.NoError(t, err)
	_, err = auth.ParseToken(userToken, auth.RoleAdmin, auth.TokenTypeAccess)
	assert.Error(t, err, "admin routes must reject tokens signed with the user key")
}

func TestInitAuthKeys_AdminKeysAreOptional(t *testing.T) {
	withJWTKeys(t, writeKeyPair(t, t.TempDir(), "user"), config.JWTKeyPaths{})

	require.NoError(t, initAuthKeys())

	assert.True(t, auth.KeysLoaded(auth.RoleUser))
	assert.False(t, auth.KeysLoaded(auth.RoleAdmin), "without admin keys, admin routes reject every token")
}

func TestInitAuthKeys_RejectsInvalidConfigurations(t *testing.T) {
	dir := t.TempDir()
	user := writeKeyPair(t, dir, "user")
	admin := writeKeyPair(t, dir, "admin")

	cases := map[string][2]config.JWTKeyPaths{
		"no user keys":               {config.JWTKeyPaths{}, admin},
		"missing user file":          {config.JWTKeyPaths{PrivateKey: config.KeyPath(filepath.Join(dir, "missing.pem")), PublicKey: user.PublicKey}, admin},
		"half-configured admin":      {user, config.JWTKeyPaths{PrivateKey: admin.PrivateKey}},
		"admin reuses the user keys": {user, user},
		"missing retired key": {config.JWTKeyPaths{
			PrivateKey: user.PrivateKey, PublicKey: user.PublicKey,
			RetiredPublicKeys: []config.KeyPath{config.KeyPath(filepath.Join(dir, "missing.pem"))},
		}, admin},
	}
	for name, keys := range cases {
		t.Run(name, func(t *testing.T) {
			withJWTKeys(t, keys[0], keys[1])
			assert.Error(t, initAuthKeys())
		})
	}
}
