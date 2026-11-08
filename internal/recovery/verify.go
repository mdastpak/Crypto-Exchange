package recovery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4/source"
	_ "github.com/golang-migrate/migrate/v4/source/file" // file:// migrations
)

// Options are what Verify checks a restored database against.
type Options struct {
	// Schema is the migration version the binary migrates to; see
	// LatestMigration. Zero skips the check.
	Schema uint
	// NotBefore, when set, is a time the restore must have reached: the
	// newest audit event is at or after it. A drill sets it to the time of
	// a change made just before the backup point it restores.
	NotBefore *time.Time
	// NotAfter, when set, is a time the restore must stop before: no audit
	// event is later. Restoring to just before an accidental DELETE or a
	// bad migration sets it to the time of that change.
	NotAfter *time.Time
	// Keys, when set, unwraps every data key and returns how many it
	// unwrapped: keys.Store.Verify with the provider of the matching KEK.
	Keys func(context.Context) (int, error)
}

// Check is the outcome of one check.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Report is what Verify found.
type Report struct {
	Checks []Check
	// Counts are the rows of the main tables, to compare with the source
	// of the backup at the same point in time.
	Counts []Count
	// NewestEvent is the time of the newest audit event: roughly the point
	// in time the database was restored to.
	NewestEvent *time.Time
}

// Count is the number of rows of a table.
type Count struct {
	Table string
	Rows  int64
}

// OK reports whether every check passed.
func (r Report) OK() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// Print writes the report, one line per check and per count.
func (r Report) Print(w io.Writer) error {
	var b strings.Builder
	for _, c := range r.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "%s %s: %s\n", mark, c.Name, c.Detail)
	}
	for _, c := range r.Counts {
		fmt.Fprintf(&b, "rows %s: %d\n", c.Table, c.Rows)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// counted are the tables whose rows a drill compares with the source.
var counted = []string{
	"users", "user_contacts", "sessions", "currencies", "balances", "balance_changes",
	"audit_events", "outbox_events", "data_keys", "platform_controls",
}

// Verify checks the database db is connected to, which was just restored.
// It only reads. It returns the report, and ErrNotVerified when a check
// failed; any other error means it could not check.
func Verify(ctx context.Context, db *sql.DB, opts Options) (Report, error) {
	var r Report
	steps := []func(context.Context, *sql.DB, Options, *Report) error{
		checkSchema, checkConstraints, checkIndexes, checkBalances, checkAuditLog, checkPointInTime, checkKeys,
	}
	for _, step := range steps {
		if err := step(ctx, db, opts, &r); err != nil {
			return r, err
		}
	}
	for _, table := range counted {
		var rows int64
		// The table names are the constants above.
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&rows); err != nil {
			return r, fmt.Errorf("counting %s: %w", table, err)
		}
		r.Counts = append(r.Counts, Count{Table: table, Rows: rows})
	}
	if !r.OK() {
		return r, ErrNotVerified
	}
	return r, nil
}

func (r *Report) add(name string, ok bool, format string, args ...any) {
	r.Checks = append(r.Checks, Check{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)})
}

// checkSchema checks that the restored schema is the binary's, and that no
// migration was cut off by the backup.
func checkSchema(ctx context.Context, db *sql.DB, opts Options, r *Report) error {
	var version int64
	var dirty bool
	err := db.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		r.add("schema", false, "schema_migrations is empty: this is not a database of the service")
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading schema_migrations: %w", err)
	}
	switch {
	case dirty:
		r.add("schema", false, "migration %d is dirty: the backup was taken while it ran", version)
	case opts.Schema != 0 && uint(version) != opts.Schema: //nolint:gosec // G115: migration versions are small and positive
		r.add("schema", false, "version %d, but this binary migrates to %d: restore with the release that matches the backup", version, opts.Schema)
	default:
		r.add("schema", true, "version %d, clean", version)
	}
	return nil
}

// checkConstraints checks that every constraint holds for every row: a
// constraint added NOT VALID, or one a restore tool skipped, does not.
func checkConstraints(ctx context.Context, db *sql.DB, _ Options, r *Report) error {
	names, err := strings1(ctx, db, `
		SELECT conrelid::regclass || '.' || conname FROM pg_constraint
		WHERE NOT convalidated AND connamespace = 'public'::regnamespace ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("reading the constraints: %w", err)
	}
	if len(names) > 0 {
		r.add("constraints", false, "not validated: %s", strings.Join(names, ", "))
	} else {
		r.add("constraints", true, "every constraint is validated")
	}
	return nil
}

// checkIndexes checks that no index was left invalid, as an interrupted
// CREATE INDEX CONCURRENTLY leaves it: a unique index that is invalid does
// not enforce uniqueness.
func checkIndexes(ctx context.Context, db *sql.DB, _ Options, r *Report) error {
	names, err := strings1(ctx, db, `
		SELECT i.indexrelid::regclass::text FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relnamespace = 'public'::regnamespace AND NOT (i.indisvalid AND i.indisready) ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("reading the indexes: %w", err)
	}
	if len(names) > 0 {
		r.add("indexes", false, "invalid: %s", strings.Join(names, ", "))
	} else {
		r.add("indexes", true, "every index is valid")
	}
	return nil
}

// checkBalances checks the balance invariants of #43 on every row, also
// where the constraints of migration 17 would already refuse them, since a
// restore is where a broken copy shows.
func checkBalances(ctx context.Context, db *sql.DB, _ Options, r *Report) error {
	var broken int64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM balances
		WHERE available < 0 OR locked < 0 OR pending_deposit < 0 OR total <> available + locked`).Scan(&broken)
	if err != nil {
		return fmt.Errorf("checking the balances: %w", err)
	}
	if broken > 0 {
		r.add("balances", false, "%d balances break the invariants", broken)
	} else {
		r.add("balances", true, "every balance holds its invariants")
	}
	return nil
}

// checkAuditLog checks that the triggers that keep the audit log
// append-only were restored and are enabled.
func checkAuditLog(ctx context.Context, db *sql.DB, _ Options, r *Report) error {
	names, err := strings1(ctx, db, `
		SELECT tgname FROM pg_trigger
		WHERE tgrelid = 'audit_events'::regclass AND NOT tgisinternal AND tgenabled <> 'D' ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("reading the audit triggers: %w", err)
	}
	want := []string{"audit_events_no_truncate", "audit_events_no_update_or_delete"}
	var missing []string
	for _, name := range want {
		found := false
		for _, have := range names {
			found = found || have == name
		}
		if !found {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		r.add("audit log", false, "missing or disabled triggers: %s; the log is not append-only", strings.Join(missing, ", "))
	} else {
		r.add("audit log", true, "append-only triggers are enabled")
	}
	return nil
}

// checkPointInTime checks the point in time the restore reached, by the
// newest audit event.
func checkPointInTime(ctx context.Context, db *sql.DB, opts Options, r *Report) error {
	var newest sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT MAX(occurred_at) FROM audit_events`).Scan(&newest); err != nil {
		return fmt.Errorf("reading the newest audit event: %w", err)
	}
	if newest.Valid {
		t := newest.Time.UTC()
		r.NewestEvent = &t
	}
	if opts.NotBefore == nil && opts.NotAfter == nil {
		return nil
	}
	shown := "none"
	if r.NewestEvent != nil {
		shown = r.NewestEvent.Format(time.RFC3339Nano)
	}
	switch {
	case opts.NotBefore != nil && (r.NewestEvent == nil || r.NewestEvent.Before(*opts.NotBefore)):
		r.add("point in time", false, "the newest audit event is %s, before %s: the restore stopped too early or WAL is missing",
			shown, opts.NotBefore.UTC().Format(time.RFC3339Nano))
	case opts.NotAfter != nil && r.NewestEvent != nil && r.NewestEvent.After(*opts.NotAfter):
		r.add("point in time", false, "the newest audit event is %s, after %s: the restore went past its target",
			shown, opts.NotAfter.UTC().Format(time.RFC3339Nano))
	default:
		r.add("point in time", true, "the newest audit event is %s", shown)
	}
	return nil
}

// checkKeys unwraps the data keys, when a provider is given.
func checkKeys(ctx context.Context, _ *sql.DB, opts Options, r *Report) error {
	if opts.Keys == nil {
		return nil
	}
	count, err := opts.Keys(ctx)
	if err != nil {
		r.add("data keys", false, "%v: restore the key management service from the backup that matches this one", err)
		return nil
	}
	r.add("data keys", true, "%d keys unwrap with the configured KEK", count)
	return nil
}

// strings1 returns the first column of every row of query.
func strings1(ctx context.Context, db *sql.DB, query string) ([]string, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LatestMigration returns the newest version of the migrations at url, such
// as file://internal/db/migrations: the schema version the binary migrates
// to.
func LatestMigration(url string) (uint, error) {
	src, err := source.Open(url)
	if err != nil {
		return 0, fmt.Errorf("opening the migrations: %w", err)
	}
	defer src.Close()
	version, err := src.First()
	if err != nil {
		return 0, fmt.Errorf("reading the migrations: %w", err)
	}
	for {
		next, err := src.Next(version)
		if errors.Is(err, fs.ErrNotExist) {
			return version, nil
		}
		if err != nil {
			return 0, fmt.Errorf("reading the migrations: %w", err)
		}
		version = next
	}
}
