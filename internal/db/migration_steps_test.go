package db

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaQueries describe the public schema, one sorted line per object. The
// table golang-migrate keeps its version in is left out.
var schemaQueries = map[string]string{
	"columns": `SELECT table_name || '.' || column_name || ' ' || udt_name || ' null=' || is_nullable || ' default=' || COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name <> 'schema_migrations'`,
	// A down migration may restore a constraint as NOT VALID, because it
	// cannot require the rows that later versions allow to satisfy it, so
	// the validation state is not compared.
	"constraints": `SELECT conrelid::regclass || ' ' || conname || ' ' || replace(pg_get_constraintdef(c.oid), ' NOT VALID', '')
		FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace
		WHERE n.nspname = 'public' AND conrelid::regclass::text <> 'schema_migrations'`,
	"indexes": `SELECT indexdef FROM pg_indexes
		WHERE schemaname = 'public' AND tablename <> 'schema_migrations'`,
	"types": `SELECT t.typname || ' ' || string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder)
		FROM pg_type t JOIN pg_enum e ON e.enumtypid = t.oid JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = 'public' GROUP BY t.typname`,
	"triggers": `SELECT pg_get_triggerdef(t.oid) FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND NOT t.tgisinternal`,
	// Functions of extensions are left out. The baseline creates pgcrypto and
	// uuid-ossp IF NOT EXISTS, and its down migration rightly keeps them: it
	// cannot know whether they existed before, or what else uses them.
	"functions": `SELECT p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')' FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'public'
		AND NOT EXISTS (SELECT 1 FROM pg_depend d
			WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')`,
	"sequences": `SELECT sequencename FROM pg_sequences WHERE schemaname = 'public'`,
	"views":     `SELECT viewname FROM pg_views WHERE schemaname = 'public'`,
}

// schemaSnapshot returns the public schema as sorted lines per kind of
// object.
func schemaSnapshot(t *testing.T, conn *sql.DB) map[string][]string {
	t.Helper()
	snapshot := map[string][]string{}
	for kind, query := range schemaQueries {
		rows, err := conn.Query(query + " ORDER BY 1")
		require.NoError(t, err, kind)
		lines := []string{}
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			lines = append(lines, line)
		}
		require.NoError(t, rows.Err())
		rows.Close()
		snapshot[kind] = lines
	}
	return snapshot
}

// TestMigrations_EachStepIsReversible applies the migrations one at a time
// and records the schema after each. It then walks back down one step at a
// time: every down migration must restore the schema that the step before
// it left, and applying the migration again must restore its own schema.
// The full up, down, up cycle cannot catch a down migration that removes
// something an earlier migration created, because it removes everything.
func TestMigrations_EachStepIsReversible(t *testing.T) {
	m := newMigrationsTestMigrator(t)
	conn, err := sql.Open("pgx", migrationsTestDBConnStr())
	require.NoError(t, err)
	defer conn.Close()

	snapshots := []map[string][]string{schemaSnapshot(t, conn)}
	for {
		err := m.Steps(1)
		if errors.Is(err, os.ErrNotExist) {
			break // no further migration
		}
		require.NoError(t, err, "migration %d", len(snapshots))
		snapshots = append(snapshots, schemaSnapshot(t, conn))
	}
	latest := len(snapshots) - 1
	require.GreaterOrEqual(t, latest, 1)

	for version := latest; version >= 1; version-- {
		require.NoError(t, m.Steps(-1), "down migration %d", version)
		assertSameSchema(t, snapshots[version-1], schemaSnapshot(t, conn), "down migration %d must restore the schema of version %d", version, version-1)

		require.NoError(t, m.Steps(1), "up migration %d after its down migration", version)
		assertSameSchema(t, snapshots[version], schemaSnapshot(t, conn), "up migration %d after its down migration", version)

		require.NoError(t, m.Steps(-1))
	}
}

func assertSameSchema(t *testing.T, want, got map[string][]string, msgAndArgs ...interface{}) {
	t.Helper()
	for kind := range schemaQueries {
		if !assert.Equal(t, want[kind], got[kind], msgAndArgs...) {
			t.Logf("%s differ:\nmissing: %s\nextra:   %s", kind,
				strings.Join(difference(want[kind], got[kind]), "\n         "),
				strings.Join(difference(got[kind], want[kind]), "\n         "))
		}
	}
}

// difference returns the lines of a that are not in b.
func difference(a, b []string) []string {
	in := map[string]bool{}
	for _, line := range b {
		in[line] = true
	}
	var out []string
	for _, line := range a {
		if !in[line] {
			out = append(out, line)
		}
	}
	return out
}
