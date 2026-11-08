package keys

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"

	"github.com/jackc/pgx/v5/pgconn"
)

// Keyring holds the unwrapped data keys that are not retired, in memory
// only. It does not change after Load; Live swaps in a newer one.
type Keyring struct {
	keys map[Purpose][]Key // by ascending version
	// now is the clock that Current compares activation times with.
	now func() time.Time
}

// Current returns the key of purpose that new seals and ciphertexts use:
// the newest version whose activation time has passed (#233). A newer
// version that is not active yet is already loaded, so data that other
// instances write with it once it is active can be read here.
func (r *Keyring) Current(purpose Purpose) (Key, error) {
	versions := r.keys[purpose]
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	for i := len(versions) - 1; i >= 0; i-- {
		if !versions[i].activeFrom.After(now) {
			return versions[i], nil
		}
	}
	return Key{}, fmt.Errorf("%w: %s", ErrNoKey, purpose)
}

// Version returns one version of purpose, to check a seal or decrypt a value
// written with it. It returns versions that are not active yet too.
func (r *Keyring) Version(purpose Purpose, version int) (Key, error) {
	for _, key := range r.keys[purpose] {
		if key.version == version {
			return key, nil
		}
	}
	return Key{}, fmt.Errorf("%w: %s/v%d", ErrNoKey, purpose, version)
}

// newest returns the newest loaded version of purpose, active or not.
func (r *Keyring) newest(purpose Purpose) (Key, bool) {
	versions := r.keys[purpose]
	if len(versions) == 0 {
		return Key{}, false
	}
	return versions[len(versions)-1], true
}

// Info describes a stored data key, without the key.
type Info struct {
	Purpose   Purpose
	Version   int
	KEKID     string
	CreatedAt time.Time
	// ActiveFrom is when the version starts writing; until then it only
	// reads (#233).
	ActiveFrom time.Time
	RetiredAt  *time.Time
	// Current marks the version that writes now: the newest active
	// version that is not retired.
	Current bool
}

// Store keeps the data keys in the data_keys table, wrapped by Provider.
type Store struct {
	DB       *sql.DB
	Provider Provider
	// Origin is who makes the changes, which record an audit event in
	// their transaction (#195): for cmd/admin, audit.Operator. Zero is the
	// system, as for the first keys that the server creates.
	Origin audit.Event
	// ActivationDelay is how long after Rotate a new version starts
	// writing (#233). Until then every instance only reads with it, so it
	// must exceed the reload interval of the instances, with a margin for
	// a reload that fails: then every instance has loaded the version
	// before any writes with it.
	ActivationDelay time.Duration
}

// record writes the audit event of action on a key in tx. The event names
// the key and its KEK, never the key.
func (s *Store) record(ctx context.Context, tx *sql.Tx, action string, purpose Purpose, version int, changes *audit.Changes) error {
	e := audit.Of(s.Origin, action, audit.TargetDataKey, fmt.Sprintf("%s/v%d", purpose, version))
	e.Changes = changes
	if err := audit.Record(ctx, tx, e); err != nil {
		return fmt.Errorf("recording %s of data key %s/v%d: %w", action, purpose, version, err)
	}
	return nil
}

// inTx runs change in a transaction, which commits only when change, and
// the audit event it records, succeed.
func (s *Store) inTx(ctx context.Context, change func(tx *sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := change(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrConcurrentChange reports a rotation that lost a race with another one.
var ErrConcurrentChange = errors.New("another process changed the data keys at the same time; run the command again")

const uniqueViolation = "23505"

// Load unwraps every data key that is not retired. A purpose without any key
// gets version 1 first, so the first start of a new deployment creates the
// keys. Load fails when a key cannot be unwrapped: data protected with it
// could not be verified or read.
func (s *Store) Load(ctx context.Context) (*Keyring, error) {
	for _, purpose := range Purposes {
		if err := s.createFirst(ctx, purpose); err != nil {
			return nil, err
		}
	}

	rows, err := s.DB.QueryContext(ctx,
		`SELECT purpose, version, kek_id, wrapped_key, active_from FROM data_keys WHERE retired_at IS NULL ORDER BY purpose, version`)
	if err != nil {
		return nil, fmt.Errorf("reading the data keys: %w", err)
	}
	defer rows.Close()
	ring := &Keyring{keys: make(map[Purpose][]Key)}
	for rows.Next() {
		var purpose Purpose
		var version int
		var kekID string
		var wrapped []byte
		var activeFrom time.Time
		if err := rows.Scan(&purpose, &version, &kekID, &wrapped, &activeFrom); err != nil {
			return nil, fmt.Errorf("reading the data keys: %w", err)
		}
		secret, err := s.unwrap(ctx, purpose, version, kekID, wrapped)
		if err != nil {
			return nil, err
		}
		ring.keys[purpose] = append(ring.keys[purpose], Key{purpose: purpose, version: version, secret: secret, activeFrom: activeFrom})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the data keys: %w", err)
	}
	rows.Close()

	// Retire never retires the newest version. If it is retired anyway,
	// someone changed the table directly, and an older key would silently
	// become current.
	latest := make(map[Purpose]int)
	if err := s.scanLatest(ctx, latest); err != nil {
		return nil, err
	}
	for _, purpose := range Purposes {
		newest, ok := ring.newest(purpose)
		if !ok {
			return nil, fmt.Errorf("every data key of purpose %s is retired; check the data_keys table", purpose)
		}
		if newest.version != latest[purpose] {
			return nil, fmt.Errorf("the newest data key %s/v%d is retired, which the key commands never do; check the data_keys table", purpose, latest[purpose])
		}
		// Retire keeps an active version, and the first version is active
		// at once, so one is always active unless the table was changed.
		if _, err := ring.Current(purpose); err != nil {
			return nil, fmt.Errorf("no data key of purpose %s is active yet, which the key commands never leave; check the data_keys table", purpose)
		}
	}
	return ring, nil
}

// Verify unwraps every data key that is not retired, with the configured
// key-encryption key, and changes nothing: unlike Load, it never creates a
// first key. A restore drill (#236) runs it on a restored database, to prove
// that the backup of the key management service matches the database
// backup; a database restored without its KEK cannot be read. It returns the
// number of keys it unwrapped.
func (s *Store) Verify(ctx context.Context) (int, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT purpose, version, kek_id, wrapped_key FROM data_keys WHERE retired_at IS NULL ORDER BY purpose, version`)
	if err != nil {
		return 0, fmt.Errorf("reading the data keys: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var purpose Purpose
		var version int
		var kekID string
		var wrapped []byte
		if err := rows.Scan(&purpose, &version, &kekID, &wrapped); err != nil {
			return count, fmt.Errorf("reading the data keys: %w", err)
		}
		secret, err := s.unwrap(ctx, purpose, version, kekID, wrapped)
		if err != nil {
			return count, err
		}
		clear(secret)
		count++
	}
	return count, rows.Err()
}

// scanLatest reads the newest version of each purpose, retired or not.
func (s *Store) scanLatest(ctx context.Context, latest map[Purpose]int) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT purpose, MAX(version) FROM data_keys GROUP BY purpose`)
	if err != nil {
		return fmt.Errorf("reading the data keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var purpose Purpose
		var version int
		if err := rows.Scan(&purpose, &version); err != nil {
			return fmt.Errorf("reading the data keys: %w", err)
		}
		latest[purpose] = version
	}
	return rows.Err()
}

// unwrap unwraps the key of one row and checks that it belongs to the row.
func (s *Store) unwrap(ctx context.Context, purpose Purpose, version int, kekID string, wrapped []byte) ([]byte, error) {
	if kekID != s.Provider.ID() {
		return nil, fmt.Errorf("data key %s/v%d was wrapped by the key-encryption key %q, but the configured one is %q",
			purpose, version, kekID, s.Provider.ID())
	}
	plaintext, err := s.Provider.Unwrap(ctx, wrapped)
	if err != nil {
		return nil, fmt.Errorf("unwrapping data key %s/v%d: %w", purpose, version, err)
	}
	secret, err := unbind(plaintext, purpose, version)
	if err != nil {
		return nil, fmt.Errorf("data key %s/v%d: %w", purpose, version, err)
	}
	return secret, nil
}

// createFirst creates version 1 of purpose unless the purpose has a key,
// also a retired one. Instances that start at the same time create one key
// between them.
func (s *Store) createFirst(ctx context.Context, purpose Purpose) error {
	var exists bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM data_keys WHERE purpose = $1)`, purpose).Scan(&exists); err != nil {
		return fmt.Errorf("reading the data keys: %w", err)
	}
	if exists {
		return nil
	}
	wrapped, err := s.newWrappedKey(ctx, purpose, 1)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO data_keys (purpose, version, kek_id, wrapped_key) VALUES ($1, 1, $2, $3) ON CONFLICT DO NOTHING`,
			purpose, s.Provider.ID(), wrapped)
		if err != nil {
			return fmt.Errorf("storing data key %s/v1: %w", purpose, err)
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			// Another instance created it, and recorded that.
			return err
		}
		return s.record(ctx, tx, audit.ActionDataKeyCreate, purpose, 1, kekChange("", s.Provider.ID()))
	})
}

// kekChange is the change of the KEK that wraps a key.
func kekChange(before, after string) *audit.Changes {
	c := &audit.Changes{After: map[string]interface{}{"kek_id": after}}
	if before != "" {
		c.Before = map[string]interface{}{"kek_id": before}
	}
	return c
}

// newWrappedKey generates a key and wraps it, bound to purpose and version.
func (s *Store) newWrappedKey(ctx context.Context, purpose Purpose, version int) ([]byte, error) {
	secret := make([]byte, KeySize)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	wrapped, err := s.Provider.Wrap(ctx, bind(purpose, version, secret))
	if err != nil {
		return nil, fmt.Errorf("wrapping data key %s/v%d: %w", purpose, version, err)
	}
	return wrapped, nil
}

// Rotate creates the next version of purpose and returns it. Every
// instance loads it within its reload interval and reads with it at once,
// and writes with it from ActivationDelay after now (#233); the older
// versions stay readable until they are retired.
func (s *Store) Rotate(ctx context.Context, purpose Purpose) (int, error) {
	if !purpose.Valid() {
		return 0, fmt.Errorf("unknown purpose %q", purpose)
	}
	var latest int
	if err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM data_keys WHERE purpose = $1`, purpose).Scan(&latest); err != nil {
		return 0, fmt.Errorf("reading the data keys: %w", err)
	}
	version := latest + 1
	wrapped, err := s.newWrappedKey(ctx, purpose, version)
	if err != nil {
		return 0, err
	}
	if s.ActivationDelay < 0 {
		return 0, fmt.Errorf("the activation delay %v is negative", s.ActivationDelay)
	}
	// A first version has nothing written before it to read, so it is
	// active at once; otherwise the purpose would have no key that writes.
	delay := s.ActivationDelay
	if latest == 0 {
		delay = 0
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO data_keys (purpose, version, kek_id, wrapped_key, active_from)
			 VALUES ($1, $2, $3, $4, NOW() + make_interval(secs => $5))`,
			purpose, version, s.Provider.ID(), wrapped, delay.Seconds()); err != nil {
			return err
		}
		changes := kekChange("", s.Provider.ID())
		changes.After["activation_delay_seconds"] = delay.Seconds()
		return s.record(ctx, tx, audit.ActionDataKeyRotate, purpose, version, changes)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return 0, ErrConcurrentChange
	}
	if err != nil {
		return 0, fmt.Errorf("storing data key %s/v%d: %w", purpose, version, err)
	}
	return version, nil
}

// Retire stops loading one version of purpose. Run it only once nothing
// needs that version anymore: data sealed or encrypted with it can no
// longer be checked or read. The newest version cannot be retired.
func (s *Store) Retire(ctx context.Context, purpose Purpose, version int) error {
	retiredNow := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE data_keys SET retired_at = NOW()
			WHERE purpose = $1 AND version = $2 AND retired_at IS NULL
			  AND EXISTS (SELECT 1 FROM data_keys newer
			              WHERE newer.purpose = $1 AND newer.version > $2
			                AND newer.retired_at IS NULL AND newer.active_from <= NOW())`,
			purpose, version)
		if err != nil {
			return err
		}
		if rows, err := res.RowsAffected(); err != nil || rows == 0 {
			return err
		}
		retiredNow = true
		return s.record(ctx, tx, audit.ActionDataKeyRetire, purpose, version, nil)
	})
	if err != nil {
		return fmt.Errorf("retiring data key %s/v%d: %w", purpose, version, err)
	}
	if retiredNow {
		return nil
	}

	var retired, newerFrom sql.NullTime
	var latest int
	err = s.DB.QueryRowContext(ctx, `
		SELECT retired_at, (SELECT MAX(version) FROM data_keys WHERE purpose = $1),
		       (SELECT MIN(active_from) FROM data_keys WHERE purpose = $1 AND version > $2 AND retired_at IS NULL)
		FROM data_keys WHERE purpose = $1 AND version = $2`, purpose, version).Scan(&retired, &latest, &newerFrom)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s/v%d", ErrNoKey, purpose, version)
	case err != nil:
		return fmt.Errorf("retiring data key %s/v%d: %w", purpose, version, err)
	case retired.Valid:
		return fmt.Errorf("data key %s/v%d is already retired", purpose, version)
	case version == latest:
		return fmt.Errorf("data key %s/v%d is the newest version; rotate first, then retire it", purpose, version)
	default:
		at := "later"
		if newerFrom.Valid {
			at = "at " + newerFrom.Time.UTC().Format(time.RFC3339)
		}
		return fmt.Errorf("data key %s/v%d still writes: the newer version becomes active %s; retire it after that", purpose, version, at)
	}
}

// Rewrap unwraps every data key with from and wraps it again with the
// store's provider, in one transaction, and returns the number of keys. The
// keys themselves do not change, so no data needs re-sealing or
// re-encryption. Run it after the KEK was rotated in the key management
// service, with from being the same provider, so that the old KEK version
// can be disabled; or with another provider, to move to a new KEK.
// Retired keys are re-wrapped too, so they stay recoverable.
func (s *Store) Rewrap(ctx context.Context, from Provider) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT purpose, version, kek_id, wrapped_key FROM data_keys ORDER BY purpose, version FOR UPDATE`)
	if err != nil {
		return 0, fmt.Errorf("reading the data keys: %w", err)
	}
	type row struct {
		purpose Purpose
		version int
		kekID   string
		wrapped []byte
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.purpose, &r.version, &r.kekID, &r.wrapped); err != nil {
			rows.Close()
			return 0, fmt.Errorf("reading the data keys: %w", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reading the data keys: %w", err)
	}

	source := Store{Provider: from}
	for _, r := range all {
		secret, err := source.unwrap(ctx, r.purpose, r.version, r.kekID, r.wrapped)
		if err != nil {
			return 0, err
		}
		wrapped, err := s.Provider.Wrap(ctx, bind(r.purpose, r.version, secret))
		if err != nil {
			return 0, fmt.Errorf("wrapping data key %s/v%d: %w", r.purpose, r.version, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE data_keys SET kek_id = $1, wrapped_key = $2 WHERE purpose = $3 AND version = $4`,
			s.Provider.ID(), wrapped, r.purpose, r.version); err != nil {
			return 0, fmt.Errorf("storing data key %s/v%d: %w", r.purpose, r.version, err)
		}
		if err := s.record(ctx, tx, audit.ActionDataKeyRewrap, r.purpose, r.version, kekChange(r.kekID, s.Provider.ID())); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(all), nil
}

// List describes every stored data key, oldest first within each purpose.
func (s *Store) List(ctx context.Context) ([]Info, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT purpose, version, kek_id, created_at, active_from, retired_at,
		       retired_at IS NULL AND active_from <= NOW()
		       AND version = MAX(version) FILTER (WHERE retired_at IS NULL AND active_from <= NOW()) OVER (PARTITION BY purpose)
		FROM data_keys ORDER BY purpose, version`)
	if err != nil {
		return nil, fmt.Errorf("reading the data keys: %w", err)
	}
	defer rows.Close()
	var infos []Info
	for rows.Next() {
		var info Info
		var retired sql.NullTime
		var current sql.NullBool
		if err := rows.Scan(&info.Purpose, &info.Version, &info.KEKID, &info.CreatedAt, &info.ActiveFrom, &retired, &current); err != nil {
			return nil, fmt.Errorf("reading the data keys: %w", err)
		}
		if retired.Valid {
			info.RetiredAt = &retired.Time
		}
		info.Current = current.Valid && current.Bool
		infos = append(infos, info)
	}
	return infos, rows.Err()
}
