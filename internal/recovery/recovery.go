// Package recovery checks backups and restores of PostgreSQL (#236).
//
// Backups themselves are taken by the database's tools: WAL archiving and
// base backups with pgBackRest, WAL-G, or a managed service, as
// docs/disaster-recovery.md describes. This package gives operators the two
// checks that the tools cannot make for the service:
//
//   - Archiving reads pg_stat_archiver on the primary, and fails when the
//     newest archived WAL segment is older than the recovery point objective,
//     or when archiving fails. cmd/admin backup status runs it, for a cron job
//     or a monitoring probe that alerts on its exit status.
//   - Verify checks a restored database: its schema is the one the binary
//     migrates to, every constraint is validated, the balance invariants and
//     the append-only audit log survived, it reached the point in time it was
//     restored to, and, with a key provider, every data key unwraps with the
//     KEK of the matching key backup. cmd/admin restore verify runs it at the
//     end of a restore drill.
//
// Both only read. Verify never migrates the database it checks.
package recovery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Archiving is the state of WAL archiving on a primary.
type Archiving struct {
	// Mode is archive_mode: off, on, or always.
	Mode string
	// Timeout is archive_timeout: how long a WAL segment stays open on an
	// idle database before it is switched and archived. Zero never switches.
	Timeout time.Duration
	// Archived is the number of segments archived since the statistics
	// were reset, and LastArchived the time of the newest one.
	Archived     int64
	LastArchived *time.Time
	LastWAL      string
	// Failed counts the failed attempts, and LastFailed is the newest.
	Failed     int64
	LastFailed *time.Time
	// Now is the database's time when the state was read.
	Now time.Time
}

// ReadArchiving reads the state of WAL archiving of the database db is
// connected to. A standby reports its own archiver, which runs only with
// archive_mode = always, so run it on the primary.
func ReadArchiving(ctx context.Context, db *sql.DB) (Archiving, error) {
	var a Archiving
	var timeout string
	var lastWAL sql.NullString
	var lastArchived, lastFailed sql.NullTime
	err := db.QueryRowContext(ctx, `
		SELECT current_setting('archive_mode'), current_setting('archive_timeout'),
		       archived_count, last_archived_wal, last_archived_time,
		       failed_count, last_failed_time, now()
		FROM pg_stat_archiver`).Scan(&a.Mode, &timeout, &a.Archived, &lastWAL, &lastArchived,
		&a.Failed, &lastFailed, &a.Now)
	if err != nil {
		return Archiving{}, fmt.Errorf("reading pg_stat_archiver: %w", err)
	}
	a.Timeout, err = parseSetting(timeout)
	if err != nil {
		return Archiving{}, fmt.Errorf("reading archive_timeout: %w", err)
	}
	a.LastWAL = lastWAL.String
	if lastArchived.Valid {
		t := lastArchived.Time
		a.LastArchived = &t
	}
	if lastFailed.Valid {
		t := lastFailed.Time
		a.LastFailed = &t
	}
	return a, nil
}

// parseSetting parses a PostgreSQL time setting as current_setting returns
// it: a number with a unit (ms, s, min, h, d), or without one in seconds,
// which is the unit of archive_timeout.
func parseSetting(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	units := []struct {
		suffix string
		unit   time.Duration
	}{{"ms", time.Millisecond}, {"min", time.Minute}, {"s", time.Second}, {"h", time.Hour}, {"d", 24 * time.Hour}}
	unit := time.Second
	for _, u := range units {
		if strings.HasSuffix(value, u.suffix) {
			value, unit = strings.TrimSuffix(value, u.suffix), u.unit
			break
		}
	}
	var n int64
	if _, err := fmt.Sscan(value, &n); err != nil || n < 0 {
		return 0, fmt.Errorf("unexpected value %q", value)
	}
	return time.Duration(n) * unit, nil
}

// Problems returns what keeps archiving from meeting a recovery point
// objective of rpo, or nothing when it meets it:
//
//   - archiving is off, so no point in time after the last base backup can
//     be restored;
//   - archive_timeout is 0 or longer than rpo, so an idle database keeps
//     its newest changes in a segment that is not archived for longer;
//   - no segment was archived yet, or the newest is older than rpo;
//   - the newest attempt failed: archive_command keeps failing, and WAL
//     piles up on the primary until its disk fills.
func (a Archiving) Problems(rpo time.Duration) []string {
	var problems []string
	if a.Mode == "off" {
		return []string{"archive_mode is off: WAL is not archived, so no point in time after the last base backup can be restored"}
	}
	if a.Timeout == 0 || a.Timeout > rpo {
		problems = append(problems, fmt.Sprintf("archive_timeout is %v: set it to at most the RPO of %v, so that an idle database archives its changes in time", a.Timeout, rpo))
	}
	switch {
	case a.LastArchived == nil:
		problems = append(problems, "no WAL segment was archived yet")
	case a.Now.Sub(*a.LastArchived) > rpo:
		problems = append(problems, fmt.Sprintf("the newest archived WAL segment (%s) is %v old, more than the RPO of %v",
			a.LastWAL, a.Now.Sub(*a.LastArchived).Round(time.Second), rpo))
	}
	if a.LastFailed != nil && (a.LastArchived == nil || a.LastFailed.After(*a.LastArchived)) {
		problems = append(problems, fmt.Sprintf("archiving failed at %s and has not succeeded since (%d failures): check archive_command and the backup storage",
			a.LastFailed.UTC().Format(time.RFC3339), a.Failed))
	}
	return problems
}

// Report prints the state of archiving.
func (a Archiving) Report(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "archive_mode:      %s\n", a.Mode)
	fmt.Fprintf(&b, "archive_timeout:   %v\n", a.Timeout)
	last := "never"
	if a.LastArchived != nil {
		last = fmt.Sprintf("%s (%s, %v ago)", a.LastArchived.UTC().Format(time.RFC3339), a.LastWAL,
			a.Now.Sub(*a.LastArchived).Round(time.Second))
	}
	fmt.Fprintf(&b, "last archived:     %s\n", last)
	fmt.Fprintf(&b, "archived / failed: %d / %d\n", a.Archived, a.Failed)
	if a.LastFailed != nil {
		fmt.Fprintf(&b, "last failure:      %s\n", a.LastFailed.UTC().Format(time.RFC3339))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// ErrNotVerified is returned by Verify when a check fails.
var ErrNotVerified = errors.New("the restored database failed verification")
