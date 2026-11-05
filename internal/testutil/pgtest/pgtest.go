// Package pgtest gives tests a PostgreSQL database whose schema is built by
// the repository's migrations, so that tests see the constraints that
// production has. It empties databases, so it only accepts names that start
// with "testdb_", and only tests import it.
package pgtest

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

	source, err := MigrationsURL()
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s", User, Password, Host, Port, name, SSLMode)
	m, err := migrate.New(source, url)
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
