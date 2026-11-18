package sanctions

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// Version is a stored version of a list, without its entries.
type Version struct {
	ID         int64     `json:"version"`
	Provider   string    `json:"provider" example:"ofac"`
	EntryCount int       `json:"entries"`
	FetchedAt  time.Time `json:"fetched_at"`
	CheckedAt  time.Time `json:"checked_at"`
}

// ErrNoList is a provider without a stored version.
var ErrNoList = errors.New("sanctions: no version of the list is stored")

// Save stores entries, parsed from source, as the newest version of the
// provider's list, unless the newest version was parsed from the same
// source: then it only records that the list is current. One save runs at
// a time, over every instance.
func Save(ctx context.Context, db *sql.DB, provider string, source []byte, entries []Entry) (Version, bool, error) {
	digest := sha256.Sum256(source)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Version{}, false, fmt.Errorf("sanctions: save the list: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('sanctions_lists:' || $1))`, provider); err != nil {
		return Version{}, false, fmt.Errorf("sanctions: save the list: %w", err)
	}
	latest, stored, err := latest(ctx, tx, provider)
	if err != nil && !errors.Is(err, ErrNoList) {
		return Version{}, false, err
	}
	if err == nil && subtle.ConstantTimeCompare(stored, digest[:]) == 1 {
		if _, err := tx.ExecContext(ctx, `UPDATE sanctions_lists SET checked_at = NOW() WHERE id = $1`, latest.ID); err != nil {
			return Version{}, false, fmt.Errorf("sanctions: save the list: %w", err)
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.SanctionsLists, "id = $1", latest.ID); err != nil {
			return Version{}, false, err
		}
		v, _, err := latestRow(ctx, tx, provider)
		if err != nil {
			return Version{}, false, err
		}
		return v, false, tx.Commit()
	}
	body, err := encode(entries)
	if err != nil {
		return Version{}, false, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO sanctions_lists (provider, source_digest, body, entry_count)
		VALUES ($1, $2, $3, $4) RETURNING id`, provider, digest[:], body, len(entries)).Scan(&id); err != nil {
		return Version{}, false, fmt.Errorf("sanctions: save the list: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.SanctionsLists, "id = $1", id); err != nil {
		return Version{}, false, err
	}
	v, _, err := latestRow(ctx, tx, provider)
	if err != nil {
		return Version{}, false, err
	}
	return v, true, tx.Commit()
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// latest returns the newest version of the provider's list, checked
// against its seal, and its source digest, as the seal covers it.
func latest(ctx context.Context, q querier, provider string) (Version, []byte, error) {
	v, r, err := latestRow(ctx, q, provider)
	if err != nil {
		return v, nil, err
	}
	source, err := hex.DecodeString(r.Values[rowseal.SanctionsLists.Index("source_digest")].String)
	return v, source, err
}

// latestRow reads the newest version, without its body, checked against
// its seal.
func latestRow(ctx context.Context, q querier, provider string) (Version, *rowseal.Row, error) {
	t := rowseal.SanctionsLists
	r := t.NewRow()
	v := Version{Provider: provider}
	err := q.QueryRowContext(ctx, `SELECT id, entry_count, fetched_at, checked_at, `+t.Fields("")+`
		FROM sanctions_lists WHERE provider = $1 ORDER BY sanctions_lists.id DESC LIMIT 1`, provider).
		Scan(append([]any{&v.ID, &v.EntryCount, &v.FetchedAt, &v.CheckedAt}, r.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil, ErrNoList
	}
	if err != nil {
		return v, nil, fmt.Errorf("sanctions: read the list: %w", err)
	}
	if err := rowseal.Require(ctx, t, r); err != nil {
		return v, nil, err
	}
	v.FetchedAt, v.CheckedAt = v.FetchedAt.UTC(), v.CheckedAt.UTC()
	return v, r, nil
}

// Latest returns the newest version of the provider's list, without its
// entries.
func Latest(ctx context.Context, db *sql.DB, provider string) (Version, error) {
	v, _, err := latestRow(ctx, db, provider)
	return v, err
}

// Load returns the entries of version v, checked against its seal, which
// covers the body by its SHA-256. The body is checked here again, as it is
// read apart from the hash that the seal checked.
func Load(ctx context.Context, db *sql.DB, v Version) (*List, error) {
	t := rowseal.SanctionsLists
	r := t.NewRow()
	var body []byte
	if err := db.QueryRowContext(ctx, `SELECT body, `+t.Fields("")+` FROM sanctions_lists WHERE id = $1`, v.ID).
		Scan(append([]any{&body}, r.Dest()...)...); err != nil {
		return nil, fmt.Errorf("sanctions: read the list: %w", err)
	}
	if err := rowseal.Require(ctx, t, r); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != r.Values[t.Index("body")].String {
		return nil, fmt.Errorf("%w: sanctions_lists %d: the body changed while it was read", rowseal.ErrUnverified, v.ID)
	}
	entries, err := decode(body)
	if err != nil {
		return nil, err
	}
	if len(entries) != v.EntryCount {
		return nil, fmt.Errorf("sanctions: list %d has %d entries, its row says %d", v.ID, len(entries), v.EntryCount)
	}
	return NewList(v.Provider, v.ID, v.FetchedAt, entries), nil
}

func encode(entries []Entry) ([]byte, error) {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func decode(body []byte) ([]Entry, error) {
	r, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("sanctions: decode the list: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(r, 512<<20))
	if err != nil {
		return nil, fmt.Errorf("sanctions: decode the list: %w", err)
	}
	var entries []Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("sanctions: decode the list: %w", err)
	}
	return entries, nil
}
