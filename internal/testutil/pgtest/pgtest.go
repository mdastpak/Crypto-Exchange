// Package pgtest gives tests a PostgreSQL database whose schema is built by
// the repository's migrations, so that tests see the constraints that
// production has. It empties databases, so it only accepts names that start
// with "testdb_", and only tests import it.
package pgtest

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
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

// MigrationsURL returns the file URL of the repository's migrations.
func MigrationsURL() string {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "db", "migrations")
	return "file://" + filepath.ToSlash(dir)
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

// Reset creates the named test database if it does not exist and empties
// its public schema.
func Reset(t testing.TB, name string) {
	t.Helper()
	checkName(t, name)

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

// Migrated empties the named test database, applies every migration, and
// returns a connection to it that is closed when the test ends.
func Migrated(t testing.TB, name string) *sql.DB {
	t.Helper()
	Reset(t, name)

	url := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s", User, Password, Host, Port, name, SSLMode)
	m, err := migrate.New(MigrationsURL(), url)
	if err != nil {
		t.Fatalf("pgtest: starting the migrations: %v", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil {
		t.Fatalf("pgtest: applying the migrations to %s: %v", name, err)
	}

	conn := open(t, name)
	t.Cleanup(func() { conn.Close() })
	return conn
}
