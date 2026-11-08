package auth

import (
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
)

// TestLiveSession_ChecksTheAbsoluteLifetime covers the SQL of #144: with a
// lifetime, a session must also be younger than it, and a rotation never
// moves its expiry past it; without one, as in a test without
// configuration, neither applies.
func TestLiveSession_ChecksTheAbsoluteLifetime(t *testing.T) {
	saved := config.Config
	t.Cleanup(func() { config.Config = saved })

	config.Config.JWT.Sessions.MaxLifetimeMinutes = 0
	assert.Equal(t, "status = 'ACTIVE' AND expires_at > NOW()", liveSession(""))
	assert.Equal(t, "$3", cappedExpiry("$3"))
	assert.Zero(t, SessionMaxLifetime())

	config.Config.JWT.Sessions.MaxLifetimeMinutes = 43200
	assert.Equal(t, "s.status = 'ACTIVE' AND s.expires_at > NOW() AND s.created_at > NOW() - make_interval(mins => 43200)", liveSession("s"))
	assert.Equal(t, "LEAST($3, created_at + make_interval(mins => 43200))", cappedExpiry("$3"))
	assert.Equal(t, 30*24*time.Hour, SessionMaxLifetime())
}
