package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"encrypted-db/config"

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
