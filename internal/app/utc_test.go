package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProcess_WorksInUTC checks that the server works in UTC whatever the
// time zone of its host (#339): a time read from PostgreSQL, and one from
// time.Now, are encoded with Z. Without the init of main.go, time.Local is
// the host's zone, which is a location of its own even on a UTC host.
func TestProcess_WorksInUTC(t *testing.T) {
	assert.Same(t, time.UTC, time.Local)

	env := newE2EEnv(t)
	var read time.Time
	require.NoError(t, env.db.QueryRow(`SELECT TIMESTAMPTZ '2099-01-01 03:30:00+03:30'`).Scan(&read))
	encoded, err := json.Marshal(read)
	require.NoError(t, err)
	assert.Equal(t, `"2099-01-01T00:00:00Z"`, string(encoded))

	now, err := json.Marshal(time.Now())
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(string(now), `Z"`), "time.Now is in UTC: %s", now)
}
