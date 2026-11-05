package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"encrypted-db/internal/testutil/tcpproxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWithMigrationLock_ReportsAFailedUnlock loses the connection while the
// migration lock is held, so that releasing the lock fails. The failure is
// logged, and the lock does not outlive the lost session: the next migration
// gets it.
func TestWithMigrationLock_ReportsAFailedUnlock(t *testing.T) {
	direct := openTestDB(t)
	t.Cleanup(func() { direct.Close() })
	proxy, err := tcpproxy.New(testPGHost + ":" + testPGPort)
	require.NoError(t, err)
	t.Cleanup(proxy.Close)
	proxied, err := sql.Open("pgx", fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		proxy.Host(), proxy.Port(), testPGUser, testPGPassword, testPGDBName, testPGSSLMode))
	require.NoError(t, err)
	t.Cleanup(func() { proxied.Close() })

	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	err = withMigrationLock(context.Background(), proxied, func() error {
		proxy.SetDown(true)
		proxy.Cut()
		return nil
	})
	require.NoError(t, err, "the work under the lock succeeded")
	assert.Contains(t, logs.String(), "Releasing the migration lock failed")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	assert.NoError(t, withMigrationLock(ctx, direct, func() error { return nil }), "the lock ended with the lost session")
}
