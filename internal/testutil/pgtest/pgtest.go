// Package pgtest gives tests a PostgreSQL database whose schema is built by
// the repository's migrations, so that tests see the constraints that
// production has. It empties databases, so it only accepts names that start
// with "testdb_", and only tests import it.
package pgtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// The PostgreSQL server that the tests use.
const (
	Host     = "localhost"
	Port     = "5432"
	User     = "postgres"
	Password = "test"
	SSLMode  = "disable"
)

var testName = regexp.MustCompile(`^testdb_[a-z0-9_]+$`)

// validName reports an error unless name is the name of a test database.
func validName(name string) error {
	if !testName.MatchString(name) {
		return fmt.Errorf("pgtest: refusing to use database %q: test database names start with \"testdb_\" and contain only lowercase letters, digits, and underscores", name)
	}
	return nil
}

// checkName stops the test unless name is the name of a test database.
func checkName(t testing.TB, name string) {
	t.Helper()
	if err := validName(name); err != nil {
		t.Fatal(err)
	}
}

// DSN returns the connection string of the named database.
func DSN(name string) string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		Host, Port, User, Password, name, SSLMode)
}

// MigrationsURL returns the file URL of the repository's migrations. Tests run
// in the directory of their package, so the module root is the nearest
// directory above it with a go.mod file. The source path that the runtime
// reports would not do: with -trimpath it is not a file path.
func MigrationsURL() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return "file://" + filepath.ToSlash(filepath.Join(dir, "internal", "db", "migrations")), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("pgtest: no go.mod above the working directory")
		}
		dir = parent
	}
}

func open(t testing.TB, name string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("pgx", DSN(name))
	if err != nil {
		t.Fatalf("pgtest: opening %s: %v", name, err)
	}
	if err := conn.Ping(); err != nil {
		conn.Close()
		t.Fatalf("pgtest: connecting to %s: %v", name, err)
	}
	return conn
}

// The test packages empty and migrate their databases at most schemaSlots
// at a time (#338). Emptying a schema and applying the baseline each lock
// every object of the schema in one transaction. With every package doing
// it at once, as `go test` runs GOMAXPROCS packages in parallel, they
// exhaust PostgreSQL's shared lock table ("out of shared memory"); four at a
// time fit in a default one, as CI's four-core runners show. One at a time
// would also fit, but makes the suite wait on each change in turn. The tests
// themselves run in parallel.
const (
	schemaSlots = 4
	// schemaLockBase is the first of the advisory locks of the slots, in
	// the postgres database.
	schemaLockBase int64 = 0x7067746573742d00 // "pgtest-\x00"
	// schemaLockWait bounds the wait for a slot.
	schemaLockWait = 10 * time.Minute
	schemaLockPoll = 20 * time.Millisecond
)

// Lock waits for a slot, so that at most schemaSlots test processes empty
// or migrate a database at once, and holds it until unlock, or the end of
// the test. A package that empties and migrates a database without Reset or
// Migrated calls it around both. It must not be held around Reset or
// Migrated, which take one themselves.
func Lock(t testing.TB) (unlock func()) {
	t.Helper()
	admin := open(t, "postgres")
	ctx := context.Background()
	conn, err := admin.Conn(ctx)
	if err != nil {
		admin.Close()
		t.Fatalf("pgtest: reserving a connection for the schema lock: %v", err)
	}
	slot, err := takeSlot(ctx, conn)
	if err != nil {
		conn.Close()
		admin.Close()
		t.Fatalf("pgtest: taking a schema slot: %v", err)
	}
	var once sync.Once
	unlock = func() {
		once.Do(func() {
			// Closing the session releases the lock too, so an error here
			// leaves nothing held.
			_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, slot)
			conn.Close()
			admin.Close()
		})
	}
	t.Cleanup(unlock)
	return unlock
}

// takeSlot takes the first free slot on conn, polling until one is free,
// and returns its lock.
func takeSlot(ctx context.Context, conn *sql.Conn) (int64, error) {
	deadline := time.Now().Add(schemaLockWait)
	for {
		for i := int64(0); i < schemaSlots; i++ {
			var taken bool
			if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, schemaLockBase+i).Scan(&taken); err != nil {
				return 0, err
			}
			if taken {
				return schemaLockBase + i, nil
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("no slot came free within %v", schemaLockWait)
		}
		time.Sleep(schemaLockPoll)
	}
}

// Reset creates the named test database if it does not exist and empties
// its public schema, under Lock.
func Reset(t testing.TB, name string) {
	t.Helper()
	checkName(t, name)
	defer Lock(t)()
	reset(t, name)
}

func reset(t testing.TB, name string) {
	t.Helper()

	admin := open(t, "postgres")
	defer admin.Close()
	var exists bool
	if err := admin.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		t.Fatalf("pgtest: looking up %s: %v", name, err)
	}
	if !exists {
		// The name is checked above, so it is safe to put in the statement.
		if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
			t.Fatalf("pgtest: creating %s: %v", name, err)
		}
	}

	conn := open(t, name)
	defer conn.Close()
	if _, err := conn.Exec("DROP SCHEMA public CASCADE; CREATE SCHEMA public;"); err != nil {
		t.Fatalf("pgtest: emptying %s: %v", name, err)
	}
}

// Migrated empties the named test database, applies every migration, both
// under Lock, and returns a connection to it that is closed when the test
// ends.
func Migrated(t testing.TB, name string) *sql.DB {
	t.Helper()
	checkName(t, name)
	unlock := Lock(t)
	reset(t, name)

	source, err := MigrationsURL()
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("pgx5://%s:%s@%s:%s/%s?sslmode=%s", User, Password, Host, Port, name, SSLMode)
	m, err := migrate.New(source, url)
	if err != nil {
		t.Fatalf("pgtest: starting the migrations: %v", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil {
		t.Fatalf("pgtest: applying the migrations to %s: %v", name, err)
	}
	unlock()

	conn := open(t, name)
	t.Cleanup(func() { conn.Close() })
	return conn
}
