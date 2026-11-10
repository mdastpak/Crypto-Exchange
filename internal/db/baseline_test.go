package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tableRowCounts returns the number of rows of every table of the public
// schema, which is how the rows that the migrations insert are compared.
func tableRowCounts(t *testing.T, conn *sql.DB) map[string]int {
	t.Helper()
	rows, err := conn.Query(`SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'schema_migrations'`)
	require.NoError(t, err)
	var tables []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	require.NoError(t, rows.Err())
	rows.Close()

	counts := map[string]int{}
	for _, table := range tables {
		var count int
		require.NoError(t, conn.QueryRow(`SELECT count(*) FROM public."`+table+`"`).Scan(&count), table)
		counts[table] = count
	}
	return counts
}

// TestBaseline_MatchesTheMigrationChain builds one database with the whole
// chain of migrations and another with the baseline alone, and expects the
// same schema and the same rows in both (#312). The down migration of the
// baseline leaves nothing behind.
func TestBaseline_MatchesTheMigrationChain(t *testing.T) {
	conn, err := sql.Open("pgx", migrationsTestDBConnStr())
	require.NoError(t, err)
	defer conn.Close()

	chain := newTestMigratorOf(t, "migrations")
	empty := schemaSnapshot(t, conn)
	require.NoError(t, chain.Up())
	wantSchema, wantRows := schemaSnapshot(t, conn), tableRowCounts(t, conn)
	require.NotEmpty(t, wantSchema["columns"])
	chain.Close()

	baseline := newTestMigratorOf(t, "baseline")
	require.NoError(t, baseline.Up())
	assertSameSchema(t, wantSchema, schemaSnapshot(t, conn), "the baseline builds the schema of the chain")
	assert.Equal(t, wantRows, tableRowCounts(t, conn), "the baseline inserts the rows of the chain")

	require.NoError(t, baseline.Down())
	assertSameSchema(t, empty, schemaSnapshot(t, conn), "the down migration of the baseline removes all of it")
}
