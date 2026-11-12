package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/golang-migrate/migrate/v4/source"
	"github.com/jackc/pgx/v5"
)

// The two database roles (#180).
//
// The migration role owns the schema and is used only by the migrations:
// at startup in the "startup" mode of postgres.migrations.mode, and by
// "admin migrate" in the "external" mode. The runtime role, postgres.user,
// is the role of the server. It owns nothing, cannot create objects, and has
// on the tables only the privileges that grantRuntimeRole gives it after
// every migration. So neither the server nor anyone who obtains its
// credentials can drop the triggers that keep audit_events append-only,
// alter a constraint, or drop a table.

// schemaName is the schema of the service's tables.
const schemaName = "public"

// AppendOnlyTables are the tables whose rows are never changed or removed.
// The runtime role may only read and insert into them. Their triggers
// refuse UPDATE, DELETE, and truncation to every role as well; withholding
// the privileges stops the server even before the triggers run, and keeps
// the protection should a trigger ever be dropped.
var AppendOnlyTables = []string{"audit_events"}

// DeletableTables are the only tables whose rows the runtime role may
// delete (#248): outbox events, which are transport and are deleted once
// published or discarded, and favorites, a preference with no value as a
// record. Every other table keeps its rows; a removal is a mark, such as
// user_contacts.removed_at, so a compromised instance or an injected
// statement can change rows, which the audit log and the ledger checks
// record, but not erase them.
var DeletableTables = []string{"outbox_events", "user_favorites"}

// RuntimeFunctions are the functions that the runtime role may call. They
// run as their owner, the migration role, and each does one narrow change
// of the schema that a job needs: creating and dropping the partitions of
// market_candles (#268), which hold public market data that the jobs can
// fetch again.
var RuntimeFunctions = []string{
	"market_candles_ensure_partition(text, timestamptz)",
	"market_candles_drop_partitions(text, timestamptz)",
}

// migrationRole returns the user and password that the migrations connect
// with: postgres.migrations.user and password, or, when no migration role is
// configured, postgres.user and password.
func migrationRole(o Options) (user, password string) {
	pg := o.Config.Postgres
	if pg.Migrations.User == "" {
		return pg.User, pg.Password
	}
	return pg.Migrations.User, pg.Migrations.Password
}

// separateRoles reports whether the migrations and the server use different
// roles, so that the migrations must grant the runtime role its privileges.
func separateRoles(o Options) bool {
	user, _ := migrationRole(o)
	return user != o.Config.Postgres.User
}

// Migrate applies every pending migration with the migration role, while
// it holds the migration lock, and then grants the runtime role its
// privileges. It is what "admin migrate" runs, before the instances of a
// release start; in the "startup" mode, every instance runs it when it
// starts. Several callers may run it at once: the lock lets one migrate,
// and the others find nothing left to do.
func Migrate(ctx context.Context, o Options) error {
	migrations, err := openMigrations(o)
	if err != nil {
		return err
	}
	defer migrations.Close()
	if err := pingWithRetry(migrations, o.Config.Postgres.ConnectRetries, time.Duration(o.Config.Postgres.ConnectRetryDelayMs)*time.Millisecond); err != nil {
		return fmt.Errorf("error connecting to PostgreSQL with the migration role: %w", err)
	}

	wait := time.Duration(o.Config.Postgres.MigrationLockTimeoutSeconds) * time.Second
	if wait <= 0 {
		wait = time.Duration(config.DefaultPostgresMigrationLockTimeoutSeconds) * time.Second
	}
	return withMigrationLock(ctx, migrations, wait, func() error {
		if err := runMigrations(migrations, o.MigrationsPath); err != nil {
			return err
		}
		if !separateRoles(o) {
			return nil
		}
		// Under the lock too: two GRANTs on the same table at once can fail
		// with "tuple concurrently updated".
		return grantRuntimeRole(ctx, migrations, o.Config.Postgres.User)
	})
}

// grantRuntimeRole gives role exactly the privileges that the server needs
// on the schema, and takes away any others, so that running it again after
// every migration also covers the tables that the migration created, and
// undoes a privilege that was granted by hand:
//
//   - USAGE on the schema, and no CREATE;
//   - SELECT, INSERT, and UPDATE on every table, and no TRUNCATE,
//     REFERENCES, or TRIGGER;
//   - DELETE as well on the DeletableTables only;
//   - only SELECT and INSERT on the AppendOnlyTables;
//   - only SELECT on schema_migrations, for the version check of the
//     "external" mode;
//   - USAGE and SELECT on every sequence, for the identity and serial
//     columns.
//
// It runs in one transaction, so the role never has a partial set.
func grantRuntimeRole(ctx context.Context, conn *sql.DB, role string) error {
	if role == "" {
		return errors.New("postgres.user is empty: there is no runtime role to grant privileges to")
	}
	r := pgx.Identifier{role}.Sanitize()
	s := pgx.Identifier{schemaName}.Sanitize()
	statements := []string{
		"GRANT USAGE ON SCHEMA " + s + " TO " + r,
		"REVOKE CREATE ON SCHEMA " + s + " FROM " + r,
		"REVOKE ALL ON ALL TABLES IN SCHEMA " + s + " FROM " + r,
		"GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA " + s + " TO " + r,
		"REVOKE ALL ON ALL SEQUENCES IN SCHEMA " + s + " FROM " + r,
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA " + s + " TO " + r,
	}
	restricted := map[string]string{"schema_migrations": "SELECT"}
	for _, table := range AppendOnlyTables {
		restricted[table] = "SELECT, INSERT"
	}
	for _, table := range DeletableTables {
		restricted[table] = "SELECT, INSERT, UPDATE, DELETE"
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("granting the runtime role %s: %w", role, err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("granting the runtime role %s: %s: %w", role, statement, err)
		}
	}
	for table, privileges := range restricted {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, schemaName+"."+table).Scan(&exists); err != nil {
			return fmt.Errorf("granting the runtime role %s: looking up %s: %w", role, table, err)
		}
		if !exists {
			continue
		}
		t := s + "." + pgx.Identifier{table}.Sanitize()
		for _, statement := range []string{
			"REVOKE ALL ON " + t + " FROM " + r,
			"GRANT " + privileges + " ON " + t + " TO " + r,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("granting the runtime role %s: %s: %w", role, statement, err)
			}
		}
	}
	for _, function := range RuntimeFunctions {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regprocedure($1) IS NOT NULL`, schemaName+"."+function).Scan(&exists); err != nil {
			return fmt.Errorf("granting the runtime role %s: looking up %s: %w", role, function, err)
		}
		if !exists {
			continue
		}
		// The function is one of RuntimeFunctions, a constant list.
		statement := "GRANT EXECUTE ON FUNCTION " + s + "." + function + " TO " + r //nolint:gosec // G202: not user input
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("granting the runtime role %s: %s: %w", role, statement, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("granting the runtime role %s: %w", role, err)
	}
	return nil
}

// RoleProblems lists why the role of conn could change the schema or the
// append-only tables, or delete rows outside the DeletableTables, one
// problem per entry; none means that it is a proper
// runtime role. Release mode refuses to start with any (#180). It checks
// the privileges that the role has, whether granted to it, to a role it
// belongs to, or to PUBLIC.
func RoleProblems(ctx context.Context, conn *sql.DB) ([]string, error) {
	var problems []string
	var role string
	var superuser, bypassRLS, createRole, createDB bool
	if err := conn.QueryRowContext(ctx, `
		SELECT current_user, rolsuper, rolbypassrls, rolcreaterole, rolcreatedb
		FROM pg_roles WHERE rolname = current_user`).Scan(&role, &superuser, &bypassRLS, &createRole, &createDB); err != nil {
		return nil, fmt.Errorf("checking the database role: %w", err)
	}
	for _, attribute := range []struct {
		set  bool
		name string
	}{{superuser, "SUPERUSER"}, {bypassRLS, "BYPASSRLS"}, {createRole, "CREATEROLE"}, {createDB, "CREATEDB"}} {
		if attribute.set {
			problems = append(problems, "it has the "+attribute.name+" attribute")
		}
	}

	var ownsSchema, createInSchema, createInDatabase bool
	if err := conn.QueryRowContext(ctx, `
		SELECT pg_has_role(current_user, n.nspowner, 'USAGE'),
		       has_schema_privilege(current_user, n.oid, 'CREATE'),
		       has_database_privilege(current_user, current_database(), 'CREATE')
		FROM pg_namespace n WHERE n.nspname = $1`, schemaName).Scan(&ownsSchema, &createInSchema, &createInDatabase); err != nil {
		return nil, fmt.Errorf("checking the database role: %w", err)
	}
	if ownsSchema {
		problems = append(problems, "it owns the schema "+schemaName)
	}
	if createInSchema {
		problems = append(problems, "it can create objects in the schema "+schemaName)
	}
	if createInDatabase {
		problems = append(problems, "it can create schemas in the database")
	}

	owned, err := names(ctx, conn, `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND pg_has_role(current_user, c.relowner, 'USAGE')
		ORDER BY c.relname LIMIT 5`, schemaName)
	if err != nil {
		return nil, err
	}
	if len(owned) > 0 {
		problems = append(problems, "it owns tables, such as "+strings.Join(owned, ", "))
	}

	// Truncation skips the row triggers, REFERENCES lets a new constraint
	// block deletes, and TRIGGER lets the role add a trigger.
	extra, err := names(ctx, conn, `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')
		  AND (has_table_privilege(current_user, c.oid, 'TRUNCATE')
		    OR has_table_privilege(current_user, c.oid, 'REFERENCES')
		    OR has_table_privilege(current_user, c.oid, 'TRIGGER'))
		ORDER BY c.relname LIMIT 5`, schemaName)
	if err != nil {
		return nil, err
	}
	if len(extra) > 0 {
		problems = append(problems, "it has TRUNCATE, REFERENCES, or TRIGGER on tables, such as "+strings.Join(extra, ", "))
	}

	for _, table := range AppendOnlyTables {
		var changes bool
		err := conn.QueryRowContext(ctx, `
			SELECT has_table_privilege(current_user, c.oid, 'UPDATE') OR has_table_privilege(current_user, c.oid, 'DELETE')
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname = $2`, schemaName, table).Scan(&changes)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("checking the database role: %w", err)
		}
		if changes {
			problems = append(problems, "it can UPDATE or DELETE rows of the append-only table "+table)
		}
	}

	// The append-only tables are reported above.
	deletes, err := names(ctx, conn, `
		SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p') AND NOT c.relname = ANY($2)
		  AND has_table_privilege(current_user, c.oid, 'DELETE')
		ORDER BY c.relname LIMIT 5`, schemaName, append(slices.Clone(DeletableTables), AppendOnlyTables...))
	if err != nil {
		return nil, err
	}
	if len(deletes) > 0 {
		problems = append(problems, "it can DELETE rows of tables other than "+strings.Join(DeletableTables, " and ")+
			", such as "+strings.Join(deletes, ", "))
	}

	for i := range problems {
		problems[i] = "the role " + role + ": " + problems[i]
	}
	return problems, nil
}

// names returns the first column of the rows of query.
func names(ctx context.Context, conn *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("checking the database role: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// checkRole runs RoleProblems on the server's pool. In release mode, a
// problem stops the start; otherwise, as in development, where one role
// usually owns everything, it is logged once.
func checkRole(ctx context.Context, conn *sql.DB, release bool) error {
	problems, err := RoleProblems(ctx, conn)
	if err != nil {
		return err
	}
	if len(problems) == 0 {
		return nil
	}
	if release {
		return fmt.Errorf("release mode needs a runtime role that cannot change the schema (postgres.user, see docs/configuration.md#database-roles):\n  %s",
			strings.Join(problems, "\n  "))
	}
	log.Printf("WARNING: the database role of the server can change the schema, which release mode refuses (docs/configuration.md#database-roles): %s",
		strings.Join(problems, "; "))
	return nil
}

// SchemaState is the version of a database next to the migrations of this
// build.
type SchemaState struct {
	// Version is the version recorded in schema_migrations; 0 for a
	// database that was never migrated.
	Version uint
	// Dirty is set when a migration failed half-way.
	Dirty bool
	// Latest is the newest migration of this build.
	Latest uint
}

// Behind reports whether migrations of this build are missing from the
// database.
func (s SchemaState) Behind() bool { return s.Version < s.Latest }

// Ahead reports whether the database has migrations that this build does
// not know, as while a release rolls out, or after a rollback.
func (s SchemaState) Ahead() bool { return s.Version > s.Latest }

// ReadSchemaState reads the version of the database and the newest
// migration of migrationsPath.
func ReadSchemaState(ctx context.Context, conn *sql.DB, migrationsPath string) (SchemaState, error) {
	var state SchemaState
	latest, err := latestMigration(migrationsPath)
	if err != nil {
		return state, err
	}
	state.Latest = latest

	// The catalog, not to_regclass, which fails without USAGE on the
	// schema, as for a runtime role before the first migration.
	var exists bool
	if err := conn.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		               WHERE n.nspname = $1 AND c.relname = 'schema_migrations')`, schemaName).Scan(&exists); err != nil {
		return state, fmt.Errorf("reading the schema version: %w", err)
	}
	if !exists {
		return state, nil
	}
	var version int64
	err = conn.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &state.Dirty)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return state, nil
	case err != nil:
		return state, fmt.Errorf("reading the schema version: %w", err)
	}
	if version > 0 {
		state.Version = uint(version)
	}
	return state, nil
}

// latestMigration returns the version of the newest migration at url.
func latestMigration(url string) (uint, error) {
	src, err := source.Open(url)
	if err != nil {
		return 0, fmt.Errorf("opening the migrations at %s: %w", url, err)
	}
	defer src.Close()
	version, err := src.First()
	if err != nil {
		return 0, fmt.Errorf("reading the migrations at %s: %w", url, err)
	}
	for {
		next, err := src.Next(version)
		if errors.Is(err, os.ErrNotExist) {
			return version, nil
		}
		if err != nil {
			return 0, fmt.Errorf("reading the migrations at %s: %w", url, err)
		}
		version = next
	}
}

// checkSchema stops an instance of the "external" mode whose database
// misses migrations of its build, or holds a failed one: the release step
// must run "admin migrate" first. A database ahead of the build is
// accepted, with a warning, so that an instance of the previous release can
// still start, or restart, while a release rolls out; migrations must
// therefore keep working for the release before them (expand, then
// contract).
func checkSchema(ctx context.Context, conn *sql.DB, migrationsPath string) error {
	state, err := ReadSchemaState(ctx, conn, migrationsPath)
	if err != nil {
		return err
	}
	switch {
	case state.Dirty:
		return fmt.Errorf("the database schema is at version %d, which failed half-way (dirty); fix it as docs/configuration.md describes, then run \"admin migrate\"", state.Version)
	case state.Behind():
		return fmt.Errorf("the database schema is at version %d, but this build needs %d; postgres.migrations.mode is %q, so run \"admin migrate\" first, as a step of the release",
			state.Version, state.Latest, config.MigrationsModeExternal)
	case state.Ahead():
		log.Printf("WARNING: the database schema is at version %d, newer than the migrations of this build (%d); this is expected only while a release rolls out", state.Version, state.Latest)
	}
	return nil
}
