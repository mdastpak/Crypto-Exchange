// Package rowseal seals protected rows with a keyed MAC (#139), so that a
// change made outside the backend, by someone with access to the database
// alone, is detected.
//
// A seal is HMAC-SHA-256, with a row-mac data key (#138), over a canonical
// encoding of the table, the row's primary key, its row_version, and its
// protected columns. The database never holds the key, so nobody can compute
// a seal in SQL, and a seal copied to another row, or to another version of
// the same row, does not match.
//
// Every backend write of a protected column calls Reseal in the transaction
// of the write. Reads that decide access (a session check, an administrator
// check, a sign-in) call Check, which records a mismatch in
// integrity_findings and the audit log, and, in enforce mode, refuses the
// row. A background Verifier walks the tables for rows that no request
// reads.
//
// The Sealer in use is process-wide (Use, Active), as the configuration is:
// without data keys (keys.provider unset), there is none, nothing is sealed,
// and nothing is checked.
package rowseal

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/keys"
)

// The modes of keys.row_seals.mode.
const (
	// ModeReport records a row that fails its check and still serves it.
	// The verifier re-seals such rows, closing their findings as
	// report_mode, so that a release can roll out and the rows written
	// before it can be adopted. It is the mode to start with.
	ModeReport = "report"
	// ModeEnforce records a row that fails its check and refuses it, until
	// an administrator reviews the finding and re-seals the row.
	ModeEnforce = "enforce"
)

// What a check found.
const (
	CheckMissing    = "missing"
	CheckMismatch   = "mismatch"
	CheckUnknownKey = "unknown_key"
	// CheckRowMissing is a row that a sealed record says exists, and that
	// is gone, such as the password of an account whose users row records
	// one (#337). A seal cannot show a row that was deleted.
	CheckRowMissing = "row_missing"
)

// Who found it.
const (
	DetectedByRead     = "read"
	DetectedByVerifier = "verifier"
)

var (
	// ErrUnverified reports a row whose seal does not match, in enforce
	// mode. Callers treat the row as unavailable.
	ErrUnverified = errors.New("the row does not match its seal")
	// ErrNotFound reports a row or finding that does not exist.
	ErrNotFound = errors.New("not found")
)

// Keys supplies the row-mac keys. *keys.Live implements it; KeyringKeys
// adapts a *keys.Keyring.
type Keys interface {
	Current(purpose keys.Purpose) (keys.Key, error)
	Version(ctx context.Context, purpose keys.Purpose, version int) (keys.Key, error)
}

// KeyringKeys serves the keys of a fixed keyring, as cmd/admin loads it.
type KeyringKeys struct{ Ring *keys.Keyring }

// Current returns the current key of purpose.
func (k KeyringKeys) Current(purpose keys.Purpose) (keys.Key, error) { return k.Ring.Current(purpose) }

// Version returns one version of purpose.
func (k KeyringKeys) Version(_ context.Context, purpose keys.Purpose, version int) (keys.Key, error) {
	return k.Ring.Version(purpose, version)
}

// Querier runs statements: a *sql.Tx of the write, or a *sql.DB.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Sealer computes and checks seals.
type Sealer struct {
	db   *sql.DB
	keys Keys
	mode string
	// Logger receives the findings; nil is the standard logger.
	Logger *log.Logger
}

// New returns a sealer that records findings in db. mode is ModeReport or
// ModeEnforce; anything else is ModeReport.
func New(db *sql.DB, k Keys, mode string) *Sealer {
	if mode != ModeEnforce {
		mode = ModeReport
	}
	return &Sealer{db: db, keys: k, mode: mode}
}

// Mode returns ModeReport or ModeEnforce.
func (s *Sealer) Mode() string { return s.mode }

var active atomic.Pointer[Sealer]

// Use makes s the sealer of the process, or none for nil, and returns a
// function that restores the previous one, for tests.
func Use(s *Sealer) (restore func()) {
	previous := active.Swap(s)
	return func() { active.Store(previous) }
}

// Active returns the sealer of the process, or nil when rows are not sealed.
func Active() *Sealer { return active.Load() }

func (s *Sealer) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// sealTag starts the sealed encoding, so that a future format can be told
// apart, and so that a row-mac key never MACs anything else by accident.
const sealTag = "crypto-exchange/row-seal/1"

// mac computes the seal of r in t with key.
func mac(key keys.Key, t *Table, r *Row) ([]byte, error) {
	h, err := key.NewMAC()
	if err != nil {
		return nil, err
	}
	var buf []byte
	field := func(b []byte, null bool) {
		if null {
			buf = append(buf, 0)
			return
		}
		buf = append(buf, 1)
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(b))) //nolint:gosec // G115: a column value is far below 4 GiB.
		buf = append(buf, b...)
	}
	// An optional column that is NULL is left out, so that the seals made
	// before it was added still match; once it has a value, the seal covers
	// it, and a change back to NULL no longer matches.
	var sealed []int
	for i, c := range t.Columns {
		if !c.Optional || r.Values[i].Valid {
			sealed = append(sealed, i)
		}
	}
	field([]byte(sealTag), false)
	field([]byte(t.Name), false)
	field([]byte(r.Key), false)
	buf = binary.BigEndian.AppendUint64(buf, uint64(r.Version))   //nolint:gosec // G115: row_version is never negative.
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(sealed))) //nolint:gosec // G115: a handful of columns.
	for _, i := range sealed {
		field([]byte(t.Columns[i].Name), false)
		field([]byte(r.Values[i].String), !r.Values[i].Valid)
	}
	h.Write(buf)
	return h.Sum(nil), nil
}

// Verdict is the result of checking one row: empty when it matches, else
// one of the Check constants.
type Verdict string

// verdict checks r against its seal.
func (s *Sealer) verdict(ctx context.Context, t *Table, r *Row) (Verdict, error) {
	return s.verdictWith(ctx, keys.PurposeRowMAC, t, r)
}

// verdictWith checks r against its seal, made with a key of purpose.
func (s *Sealer) verdictWith(ctx context.Context, purpose keys.Purpose, t *Table, r *Row) (Verdict, error) {
	if len(r.Seal) == 0 || !r.KeyVersion.Valid {
		return CheckMissing, nil
	}
	key, err := s.keys.Version(ctx, purpose, int(r.KeyVersion.Int64))
	if errors.Is(err, keys.ErrNoKey) {
		return CheckUnknownKey, nil
	}
	if err != nil {
		return "", err
	}
	expected, err := mac(key, t, r)
	if err != nil {
		return "", err
	}
	if !hmac.Equal(expected, r.Seal) {
		return CheckMismatch, nil
	}
	return "", nil
}

// Check checks a row that a read is about to rely on. A row that matches
// its seal returns nil. One that does not is recorded as a finding, with an
// audit event when the finding is new, and returns ErrUnverified in enforce
// mode, nil in report mode. A nil sealer checks nothing.
//
// The finding is written with the sealer's own connection, outside the
// caller's transaction, so that it stays when the caller rolls back.
func (s *Sealer) Check(ctx context.Context, t *Table, r *Row) error {
	if s == nil {
		return nil
	}
	v, err := s.verdict(ctx, t, r)
	if err != nil {
		// No key service, for example: the row cannot be checked. In
		// enforce mode it is refused, like a mismatch, but not recorded.
		s.logf("SECURITY: the seal of %s %s could not be checked: %v", t.Name, r.Key, err)
		if s.mode == ModeEnforce {
			return fmt.Errorf("%w: %w", ErrUnverified, err)
		}
		return nil
	}
	if v == "" {
		return nil
	}
	s.report(ctx, t, r.Key, v, DetectedByRead)
	if s.mode == ModeEnforce {
		return fmt.Errorf("%w: %s %s: %s", ErrUnverified, t.Name, r.Key, v)
	}
	return nil
}

// Check checks r with the active sealer; see Sealer.Check.
func Check(ctx context.Context, t *Table, r *Row) error { return Active().Check(ctx, t, r) }

// Missing reports a row of t that a sealed record says exists and that a
// read did not find (#337), such as the password of an account whose users
// row records one. It records a row_missing finding, as Check records a
// mismatch, and returns ErrUnverified in enforce mode and nil in report
// mode. A nil sealer reports nothing.
func (s *Sealer) Missing(ctx context.Context, t *Table, key string) error {
	if s == nil {
		return nil
	}
	s.report(ctx, t, key, CheckRowMissing, DetectedByRead)
	if s.mode == ModeEnforce {
		return fmt.Errorf("%w: %s %s: %s", ErrUnverified, t.Name, key, CheckRowMissing)
	}
	return nil
}

// Missing reports a missing row with the active sealer; see Sealer.Missing.
func Missing(ctx context.Context, t *Table, key string) error { return Active().Missing(ctx, t, key) }

// report records a finding, and its audit event when it is new. Failures
// are logged: a finding that cannot be written must not hide the refusal.
func (s *Sealer) report(ctx context.Context, t *Table, key string, v Verdict, by string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	id, created, err := recordFinding(ctx, s.db, t, key, v, by)
	if err != nil {
		s.logf("SECURITY: %s %s fails its integrity check (%s), and the finding could not be recorded: %v", t.Name, key, v, err)
		return
	}
	if !created {
		return
	}
	s.logf("SECURITY: %s %s fails its integrity check (%s); finding %d, mode %s", t.Name, key, v, id, s.mode)
	e := audit.Of(audit.Event{}, audit.ActionIntegritySealMismatch, t.Name, key)
	e.Outcome, e.Reason = audit.Failure, string(v)
	e.Changes = &audit.Changes{After: map[string]interface{}{"finding": id, "detected_by": by, "mode": s.mode}}
	if err := audit.Record(ctx, s.db, e); err != nil {
		s.logf("Failed to record the audit event of finding %d: %v", id, err)
	}
}

// recordFinding opens a finding for the row, or counts a detection on the
// open one, and reports whether it opened one.
func recordFinding(ctx context.Context, q Querier, t *Table, key string, v Verdict, by string) (int64, bool, error) {
	var id int64
	var created bool
	err := q.QueryRowContext(ctx, `
		INSERT INTO integrity_findings (table_name, row_key, check_failed, detected_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (table_name, row_key) WHERE resolved_at IS NULL
		DO UPDATE SET last_seen_at = NOW(), seen_count = integrity_findings.seen_count + 1,
			check_failed = EXCLUDED.check_failed
		RETURNING id, xmax = 0`,
		t.Name, key, string(v), by).Scan(&id, &created)
	return id, created, err
}

// Reseal seals, in q, the rows of t that where selects: it raises their
// row_version and writes their seal with the current row-mac key. Every
// backend write of a protected column calls it in the transaction of the
// write, after the write, with a condition that names the written rows,
// such as "id = $1". A nil sealer, or no matching row, does nothing.
//
// where and its arguments come from the caller's code, never from a
// request: the values are arguments.
func (s *Sealer) Reseal(ctx context.Context, q Querier, t *Table, where string, args ...any) error {
	if s == nil {
		return nil
	}
	key, err := s.keys.Current(keys.PurposeRowMAC)
	if err != nil {
		return fmt.Errorf("seal %s: %w", t.Name, err)
	}
	// One statement locks the rows, raises their version, and returns what
	// the seal covers.
	rows, err := q.QueryContext(ctx, fmt.Sprintf(`UPDATE %s SET row_version = row_version + 1 WHERE %s RETURNING %s`,
		t.Name, where, t.Fields("")), args...)
	if err != nil {
		return fmt.Errorf("seal %s: %w", t.Name, err)
	}
	var keysOf []string
	var seals [][]byte
	for rows.Next() {
		r := t.NewRow()
		if err := rows.Scan(r.Dest()...); err != nil {
			rows.Close()
			return fmt.Errorf("seal %s: %w", t.Name, err)
		}
		seal, err := mac(key, t, r)
		if err != nil {
			rows.Close()
			return err
		}
		keysOf, seals = append(keysOf, r.Key), append(seals, seal)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("seal %s: %w", t.Name, err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("seal %s: %w", t.Name, err)
	}
	if len(keysOf) == 0 {
		return nil
	}
	_, err = q.ExecContext(ctx, fmt.Sprintf(`
		UPDATE %[1]s SET row_seal = v.seal, seal_key_version = $3
		FROM unnest($1::text[], $2::bytea[]) AS v(k, seal)
		WHERE %[1]s.%[2]s = v.k::%[3]s`, t.Name, t.Key, t.KeyType),
		keysOf, seals, key.Version())
	if err != nil {
		return fmt.Errorf("seal %s: %w", t.Name, err)
	}
	return nil
}

// Reseal seals with the active sealer; see Sealer.Reseal.
func Reseal(ctx context.Context, q Querier, t *Table, where string, args ...any) error {
	return Active().Reseal(ctx, q, t, where, args...)
}
