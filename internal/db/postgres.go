package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log"
	"net"
	"net/url"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
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
// advisory lock, ensuring exclusive access to schema-mutating DDL.
func withMigrationLock(ctx context.Context, db *sql.DB, fn func() error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire connection for migration lock: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("failed to acquire migration advisory lock: %w", err)
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

func NewPostgresService() (*PostgresService, error) {
	service, err := ConnectPostgres()
	if err != nil {
		return nil, err
	}
	db := service.DB

	// Serialize migrations via a Postgres advisory lock, so that replicas of
	// this service that start at the same time do not migrate the same
	// database simultaneously.
	if err := withMigrationLock(context.Background(), db, func() error {
		return runMigrations(db)
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("error running migrations: %w", err)
	}

	db.SetMaxOpenConns(config.Config.Postgres.MaxOpenConns)
	db.SetMaxIdleConns(config.Config.Postgres.MaxIdleConns)
	db.SetConnMaxLifetime(time.Duration(config.Config.Postgres.ConnMaxLifetime) * time.Minute)
	db.SetConnMaxIdleTime(time.Duration(config.Config.Postgres.ConnMaxIdleTime) * time.Minute)

	log.Println("PostgreSQL connected and migrations applied successfully.")

	return service, nil
}

// ConnectPostgres connects to the configured database, with the startup
// retries, and does not migrate it. A restore drill (#236) checks a restored
// database as it is: migrating it first would hide a backup taken at an
// older schema.
func ConnectPostgres() (*PostgresService, error) {
	cfg, err := PostgresConfig()
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg)

	if err := pingWithRetry(db, config.Config.Postgres.ConnectRetries, time.Duration(config.Config.Postgres.ConnectRetryDelayMs)*time.Millisecond); err != nil {
		db.Close()
		return nil, fmt.Errorf("error connecting to PostgreSQL database: %w", err)
	}
	return &PostgresService{DB: db}, nil
}

// PostgresConfig returns the connection settings of the postgres section
// (#229). It never formats a value into a connection string: the user, the
// database name, and the sslmode go into a postgres:// URL, where url.URL
// escapes each one, and the password is set on the parsed configuration
// afterwards, so it passes through no parser at all. So a value with a
// space, a quote, a backslash, or "=", such as a generated password, reaches
// the server as it is, and no value can add or override a connection
// keyword. An error never includes the password.
func PostgresConfig() (*pgx.ConnConfig, error) {
	pg := config.Config.Postgres
	dsn := url.URL{
		Scheme:   "postgres",
		User:     url.User(pg.User),
		Host:     net.JoinHostPort(pg.Host, pg.Port),
		Path:     "/" + pg.DBName,
		RawQuery: url.Values{"sslmode": {pg.SSLMode}}.Encode(),
	}
	cfg, err := pgx.ParseConfig(dsn.String())
	if err != nil {
		return nil, fmt.Errorf("invalid PostgreSQL settings (postgres.host, port, user, dbname, sslmode): %w", err)
	}
	// The fallbacks, one per address and TLS mode, take the password of
	// cfg, so setting it here covers every attempt.
	cfg.Password = pg.Password
	return cfg, nil
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

func runMigrations(db *sql.DB) error {
	// The pgx driver keeps the schema_migrations table and the advisory
	// lock of the lib/pq driver used before, so existing databases carry on.
	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	if err != nil {
		return fmt.Errorf("could not start migration driver: %w", err)
	}

	m, err := migrate.NewWithDatabaseInstance(
		config.MigrationsPath(),
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
