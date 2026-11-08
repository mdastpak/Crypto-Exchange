package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/dberr"
	"github.com/mdastpak/crypto-exchange/internal/testutil/tcpproxy"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// useAvailability sets the availability settings of #230 for one test, on
// top of usePostgres.
func useAvailability(t *testing.T, hosts []string, attrs string, statementMs, lockMs, idleMs int) {
	t.Helper()
	config.Config.Postgres.Hosts = hosts
	config.Config.Postgres.TargetSessionAttrs = attrs
	config.Config.Postgres.StatementTimeoutMs = statementMs
	config.Config.Postgres.LockTimeoutMs = lockMs
	config.Config.Postgres.IdleInTransactionTimeoutMs = idleMs
}

func TestPostgresConfig_HostsTimeoutsAndName(t *testing.T) {
	usePostgres(t, "", "", "u", "p", "d", "disable")
	useAvailability(t, []string{"pg-1.internal:5432", " pg-2.internal:6432 ", "[2001:db8::1]:5433"}, "read-write", 30000, 5000, -1)
	saved := ApplicationName
	ApplicationName = "crypto-exchange-test"
	t.Cleanup(func() { ApplicationName = saved })

	cfg, err := PostgresConfig()
	require.NoError(t, err)
	assert.Equal(t, "pg-1.internal", cfg.Host)
	assert.Equal(t, uint16(5432), cfg.Port)
	var fallbacks []string
	for _, f := range cfg.Fallbacks {
		fallbacks = append(fallbacks, fmt.Sprintf("%s:%d", f.Host, f.Port))
	}
	assert.Equal(t, []string{"pg-2.internal:6432", "2001:db8::1:5433"}, fallbacks, "every host is tried in order")
	assert.NotNil(t, cfg.ValidateConnect, "read-write refuses a node that only reads")
	assert.Equal(t, map[string]string{
		"statement_timeout": "30000", "lock_timeout": "5000", "idle_in_transaction_session_timeout": "0",
		"application_name": "crypto-exchange-test",
	}, cfg.RuntimeParams)

	useAvailability(t, nil, "any", 0, 0, 0)
	config.Config.Postgres.Host, config.Config.Postgres.Port = "db", "5432"
	cfg, err = PostgresConfig()
	require.NoError(t, err)
	assert.Nil(t, cfg.ValidateConnect)
	assert.NotContains(t, cfg.RuntimeParams, "statement_timeout", "0 leaves the server's setting")
}

// TestConnectPostgres_SkipsAStoppedFirstHost lists a host where nothing
// listens before the real one: the pool connects to the second, with the
// timeouts and the name set on the session.
func TestConnectPostgres_SkipsAStoppedFirstHost(t *testing.T) {
	usePostgres(t, "", "", testPGUser, testPGPassword, testPGDBName, testPGSSLMode)
	useAvailability(t, []string{"127.0.0.1:1", testPGHost + ":" + testPGPort}, "read-write", 30000, 5000, 60000)
	svc, err := ConnectPostgres()
	require.NoError(t, err)
	defer svc.Close()

	var statement, lock, idle, name string
	require.NoError(t, svc.DB.QueryRow(`SELECT current_setting('statement_timeout'), current_setting('lock_timeout'),
		current_setting('idle_in_transaction_session_timeout'), current_setting('application_name')`).Scan(&statement, &lock, &idle, &name))
	assert.Equal(t, []string{"30s", "5s", "1min", ApplicationName}, []string{statement, lock, idle, name})
}

// TestConnectPostgres_RecoversAfterTheFirstHostStops runs a pool on the
// first of two hosts, stops it as a failed primary, and checks that the
// pool continues on the second without a restart.
func TestConnectPostgres_RecoversAfterTheFirstHostStops(t *testing.T) {
	first, err := tcpproxy.New(testPGHost + ":" + testPGPort)
	require.NoError(t, err)
	t.Cleanup(first.Close)
	second, err := tcpproxy.New(testPGHost + ":" + testPGPort)
	require.NoError(t, err)
	t.Cleanup(second.Close)

	usePostgres(t, "", "", testPGUser, testPGPassword, testPGDBName, testPGSSLMode)
	useAvailability(t, []string{first.Host() + ":" + first.Port(), second.Host() + ":" + second.Port()}, "read-write", 0, 0, 0)
	svc, err := ConnectPostgres()
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.DB.Ping())

	first.SetDown(true)
	first.Cut()
	// database/sql retries a query on a connection that turned out to be
	// closed with a new one, which the driver opens on the second host.
	require.Eventually(t, func() bool {
		var one int
		return svc.DB.QueryRow(`SELECT 1`).Scan(&one) == nil
	}, 10*time.Second, 50*time.Millisecond, "the pool reconnects to the second host")
	var one int
	require.NoError(t, svc.DB.QueryRow(`SELECT 1`).Scan(&one))
}

// TestPool_DiscardsAConnectionThatOnlyReads makes the one connection of a
// pool read-only, as on a demoted primary, and checks that the pool
// replaces it before its next use instead of failing every write on it.
func TestPool_DiscardsAConnectionThatOnlyReads(t *testing.T) {
	usePostgres(t, testPGHost, testPGPort, testPGUser, testPGPassword, testPGDBName, testPGSSLMode)
	useAvailability(t, nil, "read-write", 0, 0, 0)
	svc, err := ConnectPostgres()
	require.NoError(t, err)
	defer svc.Close()
	svc.DB.SetMaxOpenConns(1)
	ctx := context.Background()

	var pid int
	require.NoError(t, svc.DB.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	_, err = svc.DB.Exec(`SET default_transaction_read_only = on`)
	require.NoError(t, err)

	var readOnly string
	var next int
	require.NoError(t, svc.DB.QueryRowContext(ctx, `SELECT current_setting('transaction_read_only'), pg_backend_pid()`).Scan(&readOnly, &next))
	assert.Equal(t, "off", readOnly)
	assert.NotEqual(t, pid, next, "the read-only connection was replaced")

	// A write that fails as read-only marks its connection too.
	conn, err := svc.DB.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `BEGIN READ ONLY; CREATE TEMP TABLE x (id int)`)
	require.Error(t, err)
	assert.True(t, dberr.IsReadOnly(err), "%v", err)
	_, _ = conn.ExecContext(ctx, `ROLLBACK`)
	require.NoError(t, conn.Close())
	require.NoError(t, svc.DB.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid))
	assert.NotEqual(t, next, pid, "the marked connection was replaced")
}

// TestLockTimeout_IsNotedOnTheRequest blocks an update behind a row lock:
// it fails after lock_timeout, and the request's note says the database was
// busy, which the response helpers answer with 503.
func TestLockTimeout_IsNotedOnTheRequest(t *testing.T) {
	usePostgres(t, testPGHost, testPGPort, testPGUser, testPGPassword, testPGDBName, testPGSSLMode)
	useAvailability(t, nil, "read-write", 0, 200, 0)
	svc, err := ConnectPostgres()
	require.NoError(t, err)
	defer svc.Close()
	ctx := context.Background()

	_, err = svc.DB.Exec(`CREATE TABLE IF NOT EXISTS lock_timeout_test (id int PRIMARY KEY); INSERT INTO lock_timeout_test VALUES (1) ON CONFLICT DO NOTHING`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = svc.DB.Exec(`DROP TABLE IF EXISTS lock_timeout_test`) })
	holder, err := svc.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer holder.Rollback()
	_, err = holder.Exec(`SELECT id FROM lock_timeout_test WHERE id = 1 FOR UPDATE`)
	require.NoError(t, err)

	request := dberr.Track(ctx)
	started := time.Now()
	_, err = svc.DB.ExecContext(request, `UPDATE lock_timeout_test SET id = 1 WHERE id = 1`)
	require.Error(t, err)
	assert.Less(t, time.Since(started), 3*time.Second, "the wait ended at lock_timeout")
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	assert.Equal(t, "55P03", pgErr.Code)
	assert.True(t, dberr.Busy(request))
	assert.False(t, dberr.Busy(dberr.Track(ctx)), "another request is not noted")
}

// TestWithMigrationLock_StopsAndNamesTheHolder holds the migration lock in
// another session and checks that a start gives up after its limit, with
// the holder's application name in the error.
func TestWithMigrationLock_StopsAndNamesTheHolder(t *testing.T) {
	savedPoll := migrationLockPoll
	migrationLockPoll = 50 * time.Millisecond
	t.Cleanup(func() { migrationLockPoll = savedPoll })

	// The same lock held in another database, as the migrations of another
	// service or of a parallel test package do, does not block this one and
	// is not named.
	usePostgres(t, testPGHost, testPGPort, testPGUser, testPGPassword, "postgres", testPGSSLMode)
	saved := ApplicationName
	ApplicationName = "other-database"
	otherPool, err := ConnectPostgres()
	ApplicationName = saved
	require.NoError(t, err)
	defer otherPool.Close()
	other, err := otherPool.DB.Conn(context.Background())
	require.NoError(t, err)
	defer other.Close()
	_, err = other.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, migrationLockID)
	require.NoError(t, err)

	usePostgres(t, testPGHost, testPGPort, testPGUser, testPGPassword, testPGDBName, testPGSSLMode)
	ApplicationName = "lock-holder-test"
	holderPool, err := ConnectPostgres()
	ApplicationName = saved
	require.NoError(t, err)
	defer holderPool.Close()
	ctx := context.Background()
	holder, err := holderPool.DB.Conn(ctx)
	require.NoError(t, err)
	defer holder.Close()
	_, err = holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID)
	require.NoError(t, err)

	waiter := openTestDB(t)
	defer waiter.Close()
	ran := false
	started := time.Now()
	err = withMigrationLock(ctx, waiter, 300*time.Millisecond, func() error { ran = true; return nil })
	require.Error(t, err)
	assert.False(t, ran)
	assert.Less(t, time.Since(started), 5*time.Second)
	assert.Contains(t, err.Error(), "application_name lock-holder-test")
	assert.True(t, strings.Contains(err.Error(), "pid "), err.Error())

	_, err = holder.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, migrationLockID)
	require.NoError(t, err)
	require.NoError(t, withMigrationLock(ctx, waiter, time.Second, func() error { ran = true; return nil }))
	assert.True(t, ran, "once the holder releases it, the lock is taken")
}

func TestServerApplicationName(t *testing.T) {
	assert.True(t, strings.HasPrefix(ServerApplicationName(), "crypto-exchange-server/"))
}
