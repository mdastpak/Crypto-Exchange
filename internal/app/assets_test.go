package app

import (
	"context"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSyncAssets checks assets.auto_sync at startup (#57): off, nothing is
// imported; on without a path, the registry built into the release is,
// once; a registry that cannot be read keeps the server from starting.
func TestSyncAssets(t *testing.T) {
	ctx := context.Background()
	db := pgtest.Migrated(t, "testdb_app_assets")
	currencies := func() int {
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM currencies`).Scan(&n))
		return n
	}
	var settings config.Configuration

	added, err := syncAssets(ctx, db, &settings)
	require.NoError(t, err)
	assert.False(t, added)
	assert.Zero(t, currencies(), "off by default")

	settings.Assets.AutoSync = true
	added, err = syncAssets(ctx, db, &settings)
	require.NoError(t, err)
	assert.True(t, added)
	assert.Positive(t, currencies(), "the registry of the release")

	added, err = syncAssets(ctx, db, &settings)
	require.NoError(t, err)
	assert.False(t, added, "the next start, or another instance, finds nothing to add")

	settings.Assets.RegistryPath = "no-such-registry.yaml"
	_, err = syncAssets(ctx, db, &settings)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "assets.auto_sync")
}
