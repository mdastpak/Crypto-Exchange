package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartGeoIP checks the start of the resolver (#284): off by default,
// a database that does not load stops a release but not development, and
// the reloader stops with the server.
func TestStartGeoIP(t *testing.T) {
	saved := config.Config
	t.Cleanup(func() { config.Config = saved })

	config.Config = config.Configuration{}
	resolver, w, err := startGeoIP()
	require.NoError(t, err)
	assert.Nil(t, resolver)
	assert.Nil(t, w)

	g := &config.Config.GeoIP
	g.Enabled, g.DatabasePath = true, filepath.Join(t.TempDir(), "missing.mmdb")
	g.ReloadIntervalSeconds, g.MaxAgeDays = 10, 45
	config.Config.Server.Mode = "release"
	_, _, err = startGeoIP()
	assert.Error(t, err, "a release does not start without its database")

	config.Config.Server.Mode = "debug"
	resolver, w, err = startGeoIP()
	require.NoError(t, err)
	require.NotNil(t, resolver)
	require.NotNil(t, w)
	assert.Error(t, resolver.Check(time.Now()), "the missing database is reported")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	assert.NoError(t, w.stop(ctx))

	// The header alone needs no reloader.
	g.DatabasePath, g.Header = "", "CF-IPCountry"
	config.Config.Server.TrustedProxies = []string{"10.0.0.0/8"}
	resolver, w, err = startGeoIP()
	require.NoError(t, err)
	assert.NotNil(t, resolver)
	assert.Nil(t, w)
}
