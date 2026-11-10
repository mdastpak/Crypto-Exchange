package db

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSchema_EveryTimeHasATimeZone checks that no column of the schema is a
// timestamp without a time zone, whose value depends on the session time
// zone of whoever wrote it (#196, #315).
func TestSchema_EveryTimeHasATimeZone(t *testing.T) {
	conn := pgtest.Migrated(t, "testdb_timestamps")
	rows, err := conn.Query(`SELECT table_name || '.' || column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND data_type = 'timestamp without time zone' ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()
	var naive []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		naive = append(naive, name)
	}
	require.NoError(t, rows.Err())
	assert.Empty(t, naive)

	var nullable []string
	rows, err = conn.Query(`SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'users' AND column_name IN ('created_at', 'updated_at') AND is_nullable = 'YES'`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		nullable = append(nullable, name)
	}
	assert.Empty(t, nullable, "users always have a creation and an update time")
}
