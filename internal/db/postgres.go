package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
)

type PostgresService struct {
	DB *sql.DB
}

// migrationLockID is a fixed Postgres advisory lock key used to serialize
// schema migrations against a database. This guards against multiple
// concurrently-starting replicas of this service racing DDL against the same
// database at once.
const migrationLockID int64 = 918273645

// withMigrationLock runs fn while holding a session-scoped Postgres
// advisory lock, ensuring exclusive access to schema-mutating DDL. It waits
// for the lock at most wait (#230): an instance whose migration hangs must
// not stall every other instance that starts, without a word. While it
// waits, it logs the session that holds the lock, and when the wait ends,
// it fails with that session in the error.
func withMigrationLock(ctx context.Context, db *sql.DB, wait time.Duration, fn func() error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire connection for migration lock: %w", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(wait)
	nextLog := time.Now().Add(migrationLockLogEvery)
	for {
		var locked bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", migrationLockID).Scan(&locked); err != nil {
			return fmt.Errorf("failed to acquire migration advisory lock: %w", err)
		}
		if locked {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the migration lock is still held after %v, by %s; check that session, which may run a migration that hangs, then start again (postgres.migration_lock_timeout_seconds sets the wait)",
				wait, migrationLockHolder(ctx, conn))
		}
		if time.Now().After(nextLog) {
			log.Printf("Waiting for the migration lock, held by %s", migrationLockHolder(ctx, conn))
			nextLog = time.Now().Add(migrationLockLogEvery)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(migrationLockPoll):
		}
	}
	defer func() {
		if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockID); err != nil {
			// The lock belongs to the database session, so a connection
			// that may still hold it must not go back to the pool, where it
			// would block every later migration.
			log.Printf("Releasing the migration lock failed; discarding the connection: %v", err)
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()

	return fn()
}

// How often withMigrationLock tries the lock, and logs its holder.
var (
	migrationLockPoll     = 500 * time.Millisecond
	migrationLockLogEvery = 10 * time.Second
)

// migrationLockHolder describes the session that holds the migration lock:
// its process ID, application name, client address, and how long it has
// held its current transaction, for the operator to find it.
func migrationLockHolder(ctx context.Context, conn *sql.Conn) string {
	var pid int
	var app, addr, state string
	var since sql.NullTime
	err := conn.QueryRowContext(ctx, `
		SELECT a.pid, COALESCE(NULLIF(a.application_name, ''), '-'), COALESCE(host(a.client_addr), 'local'),
		       COALESCE(a.state, '-'), COALESCE(a.xact_start, a.backend_start)
		FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted
		  -- An advisory lock belongs to one database; the server may hold the
		  -- same key in others, such as the databases of other services.
		  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND l.classid = ($1::bigint >> 32)::oid AND l.objid = ($1::bigint & 4294967295)::oid AND l.objsubid = 1
		LIMIT 1`, migrationLockID).Scan(&pid, &app, &addr, &state, &since)
	if err != nil {
		return "a session that could not be identified (" + err.Error() + ")"
	}
	held := "-"
	if since.Valid {
		held = time.Since(since.Time).Round(time.Second).String()
	}
	return fmt.Sprintf("pid %d (application_name %s, client %s, state %s, for %s)", pid, app, addr, state, held)
}

// NewPostgresService connects the server's pool, with the runtime role of
// postgres.user, and makes sure that the schema is current (#180): in the
// "startup" mode of postgres.migrations.mode, it migrates first, with the
// migration role, under the migration lock; in the "external" mode, it
// only checks that the release step migrated the database. It then checks
// the runtime role, which release mode refuses when the role could change
// the schema.
func NewPostgresService(o Options) (*PostgresService, error) {
	ctx := context.Background()
	pg := o.Config.Postgres
	if pg.Migrations.Mode != config.MigrationsModeExternal {
		// Migrations run on connections of their own, without the timeouts
		// of the pool, which would stop a long migration, and with their
		// own application_name. The lock serializes the replicas of this
		// service that start at the same time.
		if err := Migrate(ctx, o); err != nil {
			return nil, fmt.Errorf("error running migrations: %w", err)
		}
	}

	service, err := ConnectPostgres(o)
	if err != nil {
		return nil, err
	}
	db := service.DB
	if pg.Migrations.Mode == config.MigrationsModeExternal {
		if err := checkSchema(ctx, db, o.MigrationsPath); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := checkRole(ctx, db, o.release()); err != nil {
		db.Close()
		return nil, err
	}

	db.SetMaxOpenConns(pg.MaxOpenConns)
	db.SetMaxIdleConns(pg.MaxIdleConns)
	db.SetConnMaxLifetime(time.Duration(pg.ConnMaxLifetime) * time.Minute)
	db.SetConnMaxIdleTime(time.Duration(pg.ConnMaxIdleTime) * time.Minute)

	log.Println("PostgreSQL connected and the schema is current.")

	return service, nil
}

// ConnectPostgres connects to the configured database, with the startup
// retries, and does not migrate it. A restore drill (#236) checks a restored
// database as it is: migrating it first would hide a backup taken at an
// older schema.
func ConnectPostgres(o Options) (*PostgresService, error) {
	cfg, err := PostgresConfig(o)
	if err != nil {
		return nil, err
	}
	db := openPool(cfg, o.Config.Postgres.TargetSessionAttrs)

	if err := pingWithRetry(db, o.Config.Postgres.ConnectRetries, time.Duration(o.Config.Postgres.ConnectRetryDelayMs)*time.Millisecond); err != nil {
		db.Close()
		return nil, fmt.Errorf("error connecting to PostgreSQL database: %w", err)
	}
	return &PostgresService{DB: db}, nil
}

// PostgresConfig returns the connection settings of the postgres section
// (#229, #230). It never formats a value into a connection string: the
// user, the database name, the hosts, and the parameters go into a
// postgres:// URL, where url.URL escapes each one, and the password is set
// on the parsed configuration afterwards, so it passes through no parser at
// all. So a value with a space, a quote, a backslash, or "=", such as a
// generated password, reaches the server as it is, and no value can add or
// override a connection keyword. An error never includes the password.
//
// The configuration names every host of postgres.hosts, or host and port,
// with target_session_attrs, and sets the server-side timeouts and the
// application_name of ApplicationName as run-time parameters.
func PostgresConfig(o Options) (*pgx.ConnConfig, error) {
	pg := o.Config.Postgres
	return connConfig(o, pg.User, pg.Password, pg.StatementTimeoutMs, pg.LockTimeoutMs, pg.IdleInTransactionTimeoutMs, ApplicationName)
}

// connConfig builds the configuration of the role user, with the given
// timeouts and name.
func connConfig(o Options, user, password string, statementMs, lockMs, idleMs int, name string) (*pgx.ConnConfig, error) {
	pg := o.Config.Postgres
	hosts := net.JoinHostPort(pg.Host, pg.Port)
	if len(pg.Hosts) > 0 {
		trimmed := make([]string, len(pg.Hosts))
		for i, h := range pg.Hosts {
			trimmed[i] = strings.TrimSpace(h)
		}
		hosts = strings.Join(trimmed, ",")
	}
	params := url.Values{"sslmode": {pg.SSLMode}}
	if pg.TargetSessionAttrs != "" {
		params.Set("target_session_attrs", pg.TargetSessionAttrs)
	}
	for key, ms := range map[string]int{
		"statement_timeout":                   statementMs,
		"lock_timeout":                        lockMs,
		"idle_in_transaction_session_timeout": idleMs,
	} {
		switch {
		case ms > 0:
			params.Set(key, strconv.Itoa(ms))
		case ms < 0:
			params.Set(key, "0") // turned off
		}
	}
	if name != "" {
		params.Set("application_name", name)
	}
	dsn := url.URL{
		Scheme:   "postgres",
		User:     url.User(user),
		Host:     hosts,
		Path:     "/" + pg.DBName,
		RawQuery: params.Encode(),
	}
	cfg, err := pgx.ParseConfig(dsn.String())
	if err != nil {
		return nil, fmt.Errorf("invalid PostgreSQL settings (postgres.host, port, hosts, user, dbname, sslmode, target_session_attrs): %w", err)
	}
	// The fallbacks, one per address and TLS mode, take the password of
	// cfg, so setting it here covers every attempt.
	cfg.Password = password
	cfg.Tracer = busyTracer{}
	return cfg, nil
}

// openMigrations opens the connections that migrations run on, with the
// migration role (#180), without statement or lock timeouts, named
// crypto-exchange-migrate. The advisory lock holds one connection, and
// golang-migrate takes others, so the pool is small but not one.
func openMigrations(o Options) (*sql.DB, error) {
	pg := o.Config.Postgres
	user, password := migrationRole(o)
	cfg, err := connConfig(o, user, password, -1, -1, pg.IdleInTransactionTimeoutMs, migrationApplicationName())
	if err != nil {
		return nil, err
	}
	db := openPool(cfg, o.Config.Postgres.TargetSessionAttrs)
	db.SetMaxOpenConns(4)
	return db, nil
}

func (p *PostgresService) Close() error {
	if p.DB != nil {
		if err := p.DB.Close(); err != nil {
			log.Printf("Error closing PostgreSQL connection: %v", err)
			return err
		}
	}
	return nil
}

func (p *PostgresService) Ping(ctx context.Context) error {
	if p.DB == nil {
		return fmt.Errorf("database not initialized")
	}
	return p.DB.PingContext(ctx)
}

func (p *PostgresService) Stats() sql.DBStats {
	if p.DB == nil {
		return sql.DBStats{}
	}
	return p.DB.Stats()
}

func runMigrations(db *sql.DB, migrationsPath string) error {
	// The pgx driver keeps the schema_migrations table and the advisory
	// lock of the lib/pq driver used before, so existing databases carry on.
	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		return fmt.Errorf("could not start migration driver: %w", err)
	}

	m, err := migrate.NewWithDatabaseInstance(
		migrationsPath,
		"pgx5", driver)
	if err != nil {
		return fmt.Errorf("migration instance creation failed: %w", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("migration failed: %w", err)
	}

	return nil
}

// pingWithRetry pings the database up to maxAttempts times with an exponential
// backoff between attempts, so transient startup ordering issues (e.g. the
// database container not yet accepting connections) do not fail the service.
// A maxAttempts value <= 1 performs a single attempt with no retry.
func pingWithRetry(db *sql.DB, maxAttempts int, delay time.Duration) error {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if delay <= 0 {
		delay = 500 * time.Millisecond
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		lastErr = db.PingContext(ctx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if attempt < maxAttempts {
			log.Printf("PostgreSQL ping attempt %d/%d failed: %v, retrying in %v", attempt, maxAttempts, lastErr, delay)
			time.Sleep(delay)
			delay *= 2
		}
	}
	return lastErr
}

// Options are what the package reads of the configuration of its process,
// which the composition root passes (#48): the package reads no global
// configuration.
type Options struct {
	// Config is the configuration of the process.
	Config *config.Configuration
	// MigrationsPath is the source URL of the migrations, such as
	// file://internal/db/migrations.
	MigrationsPath string
}

// release reports whether the process runs in release mode.
func (o Options) release() bool { return o.Config.Server.Mode == "release" }
