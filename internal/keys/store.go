package keys

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Keyring holds the unwrapped data keys that are not retired, in memory
// only. It does not change after Load.
type Keyring struct {
	keys map[Purpose][]Key // by ascending version
}

// Current returns the newest key of purpose, which new seals and
// ciphertexts use.
func (r *Keyring) Current(purpose Purpose) (Key, error) {
	versions := r.keys[purpose]
	if len(versions) == 0 {
		return Key{}, fmt.Errorf("%w: %s", ErrNoKey, purpose)
	}
	return versions[len(versions)-1], nil
}

// Version returns one version of purpose, to check a seal or decrypt a value
// written with it.
func (r *Keyring) Version(purpose Purpose, version int) (Key, error) {
	for _, key := range r.keys[purpose] {
		if key.version == version {
			return key, nil
		}
	}
	return Key{}, fmt.Errorf("%w: %s/v%d", ErrNoKey, purpose, version)
}

// Info describes a stored data key, without the key.
type Info struct {
	Purpose   Purpose
	Version   int
	KEKID     string
	CreatedAt time.Time
	RetiredAt *time.Time
	// Current marks the newest version that is not retired.
	Current bool
}

// Store keeps the data keys in the data_keys table, wrapped by Provider.
type Store struct {
	DB       *sql.DB
	Provider Provider
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
		`SELECT purpose, version, kek_id, wrapped_key FROM data_keys WHERE retired_at IS NULL ORDER BY purpose, version`)
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
		if err := rows.Scan(&purpose, &version, &kekID, &wrapped); err != nil {
			return nil, fmt.Errorf("reading the data keys: %w", err)
		}
		secret, err := s.unwrap(ctx, purpose, version, kekID, wrapped)
		if err != nil {
			return nil, err
		}
		ring.keys[purpose] = append(ring.keys[purpose], Key{purpose: purpose, version: version, secret: secret})
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
		current, err := ring.Current(purpose)
		if err != nil {
			return nil, fmt.Errorf("every data key of purpose %s is retired; check the data_keys table", purpose)
		}
		if current.version != latest[purpose] {
			return nil, fmt.Errorf("the newest data key %s/v%d is retired, which the key commands never do; check the data_keys table", purpose, latest[purpose])
		}
	}
	return ring, nil
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
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO data_keys (purpose, version, kek_id, wrapped_key) VALUES ($1, 1, $2, $3) ON CONFLICT DO NOTHING`,
		purpose, s.Provider.ID(), wrapped)
	if err != nil {
		return fmt.Errorf("storing data key %s/v1: %w", purpose, err)
	}
	return nil
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

// Rotate creates the next version of purpose and returns it. Each process
// uses it as the current key once it loads the keys again, after a restart;
// the older versions stay readable until they are retired.
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
	_, err = s.DB.ExecContext(ctx,
		`INSERT INTO data_keys (purpose, version, kek_id, wrapped_key) VALUES ($1, $2, $3, $4)`,
		purpose, version, s.Provider.ID(), wrapped)
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
	res, err := s.DB.ExecContext(ctx, `
		UPDATE data_keys SET retired_at = NOW()
		WHERE purpose = $1 AND version = $2 AND retired_at IS NULL
		  AND version < (SELECT MAX(version) FROM data_keys WHERE purpose = $1)`,
		purpose, version)
	if err != nil {
		return fmt.Errorf("retiring data key %s/v%d: %w", purpose, version, err)
	}
	if rows, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("retiring data key %s/v%d: %w", purpose, version, err)
	} else if rows == 1 {
		return nil
	}

	var retired sql.NullTime
	var latest int
	err = s.DB.QueryRowContext(ctx, `
		SELECT retired_at, (SELECT MAX(version) FROM data_keys WHERE purpose = $1)
		FROM data_keys WHERE purpose = $1 AND version = $2`, purpose, version).Scan(&retired, &latest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s/v%d", ErrNoKey, purpose, version)
	case err != nil:
		return fmt.Errorf("retiring data key %s/v%d: %w", purpose, version, err)
	case retired.Valid:
		return fmt.Errorf("data key %s/v%d is already retired", purpose, version)
	default:
		return fmt.Errorf("data key %s/v%d is the newest version; rotate first, then retire it", purpose, version)
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
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(all), nil
}

// List describes every stored data key, oldest first within each purpose.
func (s *Store) List(ctx context.Context) ([]Info, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT purpose, version, kek_id, created_at, retired_at,
		       retired_at IS NULL AND version = MAX(version) FILTER (WHERE retired_at IS NULL) OVER (PARTITION BY purpose)
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
		if err := rows.Scan(&info.Purpose, &info.Version, &info.KEKID, &info.CreatedAt, &retired, &current); err != nil {
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
