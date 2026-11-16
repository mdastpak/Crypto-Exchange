package db

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The rules of CONTRIBUTING.md#migrations that a test can check (#455).
// Branches write migrations in parallel, and each migration that adds a
// sealed table restates the whole list of integrity_findings_table_name_check:
// a migration written before another's would drop a table from it.

// quotedName is a table name in a constraint definition.
var quotedName = regexp.MustCompile(`'([a-z_][a-z0-9_]*)'`)

// findingTablesProblems compares the names that the definition of the
// findings check accepts with the sealed tables.
func findingTablesProblems(definition string, tables []string) (missing, extra []string) {
	accepted := map[string]bool{}
	for _, m := range quotedName.FindAllStringSubmatch(definition, -1) {
		accepted[m[1]] = true
	}
	sealed := map[string]bool{}
	for _, name := range tables {
		sealed[name] = true
		if !accepted[name] {
			missing = append(missing, name)
		}
	}
	for name := range accepted {
		if !sealed[name] {
			extra = append(extra, name)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	return missing, extra
}

func sealedTableNames() []string {
	names := make([]string, 0, len(rowseal.Tables))
	for _, t := range rowseal.Tables {
		names = append(names, t.Name)
	}
	return names
}

// TestSchema_FindingsAcceptEverySealedTable checks that the findings of
// every table of rowseal.Tables can be recorded, and that the check names
// no other table.
func TestSchema_FindingsAcceptEverySealedTable(t *testing.T) {
	conn := pgtest.Migrated(t, "testdb_migration_rules")
	var definition string
	require.NoError(t, conn.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = 'integrity_findings_table_name_check'`).Scan(&definition))
	missing, extra := findingTablesProblems(definition, sealedTableNames())
	assert.Empty(t, missing, "sealed tables whose findings the check refuses: a migration restated an older list")
	assert.Empty(t, extra, "tables that the check names but rowseal.Tables does not")
}

// TestFindingTablesProblems shows that the check above fails on a list
// that drops a table, as a migration written in parallel would.
func TestFindingTablesProblems(t *testing.T) {
	missing, extra := findingTablesProblems(`CHECK ((table_name = ANY (ARRAY['users'::text, 'sessions'::text, 'old_table'::text])))`,
		[]string{"users", "sessions", "deny_links"})
	assert.Equal(t, []string{"deny_links"}, missing)
	assert.Equal(t, []string{"old_table"}, extra)
}

// firstTimestampTZMigration is the first migration that must spell its
// time columns TIMESTAMPTZ. Migrations 35 and 37 use the long form, and a
// merged migration is never edited.
const firstTimestampTZMigration = 38

// longTimestamp is a time type other than TIMESTAMPTZ: the long forms, or a
// bare timestamp, which has no time zone.
var longTimestamp = regexp.MustCompile(`(?i)\btimestamp\b(\s*\(\d+\))?(\s+with(out)?\s+time\s+zone)?`)

// sqlComment is a comment to the end of the line.
var sqlComment = regexp.MustCompile(`--[^\n]*`)

// quotedIdentifier is a name in double quotes, such as the column
// "timestamp" of balance_changes: a name, never a type.
var quotedIdentifier = regexp.MustCompile(`"[^"]*"`)

// timestampProblems returns the lines of sql that spell a time type other
// than TIMESTAMPTZ.
func timestampProblems(sql string) []string {
	var lines []string
	for i, line := range strings.Split(sql, "\n") {
		code := quotedIdentifier.ReplaceAllString(sqlComment.ReplaceAllString(line, ""), `""`)
		if longTimestamp.MatchString(code) {
			lines = append(lines, strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
		}
	}
	return lines
}

// TestMigrations_SpellTimestampTZ checks the migrations from
// firstTimestampTZMigration on, up and down.
func TestMigrations_SpellTimestampTZ(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	checked := 0
	for _, file := range files {
		number, _, ok := strings.Cut(filepath.Base(file), "_")
		n, err := strconv.Atoi(number)
		if !ok || err != nil {
			t.Errorf("%s is not named <number>_<name>", file)
			continue
		}
		if n < firstTimestampTZMigration {
			continue
		}
		body, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Empty(t, timestampProblems(string(body)), "%s: spell time columns TIMESTAMPTZ (CONTRIBUTING.md#migrations)", file)
		checked++
	}
	assert.Positive(t, checked)
}

// TestTimestampProblems shows the forms that the check above refuses.
func TestTimestampProblems(t *testing.T) {
	for _, bad := range []string{
		"ALTER TABLE t ADD COLUMN at timestamp with time zone;",
		"created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL,",
		"created_at timestamp NOT NULL,",
		"created_at TIMESTAMP(3) WITH TIME ZONE,",
		"SELECT now()::timestamp;",
		`ALTER TABLE t ADD COLUMN "at" timestamp;`,
	} {
		assert.Len(t, timestampProblems(bad), 1, bad)
	}
	for _, good := range []string{
		"created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),",
		"approved_at timestamptz,",
		"-- a timestamp with time zone, in a comment",
		"retry_after TIMESTAMPTZ(3) NOT NULL,",
		`CREATE INDEX i ON balance_changes (user_id, "timestamp" DESC);`,
	} {
		assert.Empty(t, timestampProblems(good), good)
	}
}

// TestMigrations_ComeInPairs checks that every up migration has its down
// migration, and that no number is taken twice, as when two branches took
// the same next number.
func TestMigrations_ComeInPairs(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	require.NoError(t, err)
	byNumber := map[string][]string{}
	for _, file := range files {
		name := filepath.Base(file)
		number, _, _ := strings.Cut(name, "_")
		byNumber[number] = append(byNumber[number], name)
	}
	for number, names := range byNumber {
		slices.Sort(names)
		if assert.Len(t, names, 2, "migration %s: one up and one down, under one name", number) {
			up, down := strings.TrimSuffix(names[1], ".up.sql"), strings.TrimSuffix(names[0], ".down.sql")
			assert.Equal(t, down, up, "migration %s", number)
		}
	}
}
