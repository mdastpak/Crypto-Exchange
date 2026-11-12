// Package favorites holds the personal favorites of users (#217): the
// currencies and trading pairs that each user follows, in their order.
//
// The favorites live only in PostgreSQL, so every instance reads the same
// list. An add counts the favorites of the user in its transaction, after a
// lock of the user's row, so that concurrent adds on any instance cannot
// pass the cap together.
package favorites

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Kind is what a favorite is of.
type Kind string

const (
	KindCurrency Kind = "currency"
	KindPair     Kind = "pair"
)

// Errors of the changes.
var (
	ErrUserNotFound = errors.New("user not found")
	// ErrNotListed is an add of a currency or pair that is not listed:
	// a currency that is not approved, or a pair that is not active.
	ErrNotListed = errors.New("not listed")
	// ErrLimit is an add past the cap.
	ErrLimit = errors.New("too many favorites")
	// ErrOrderMismatch is a reorder that does not name each favorite of the
	// kind exactly once.
	ErrOrderMismatch = errors.New("the order does not name every favorite once")
)

// lockUser takes the lock of the user's row that orders the changes of the
// user's favorites on every instance.
func lockUser(ctx context.Context, tx *sql.Tx, userHK string) error {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE hk = $1 AND deleted_at IS NULL FOR UPDATE`, userHK).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to lock the user: %w", err)
	}
	return nil
}

// listed reports whether target can be added: an approved currency that is
// not deleted, or an active pair.
func listed(ctx context.Context, tx *sql.Tx, kind Kind, target string) (bool, error) {
	query := `SELECT EXISTS (SELECT 1 FROM currencies WHERE hk = $1 AND status = 'approved' AND deleted_at IS NULL)`
	if kind == KindPair {
		query = `SELECT EXISTS (SELECT 1 FROM markets WHERE id = $1 AND status = 'ACTIVE')`
	}
	var ok bool
	if err := tx.QueryRowContext(ctx, query, target).Scan(&ok); err != nil {
		return false, fmt.Errorf("failed to read the %s: %w", kind, err)
	}
	return ok, nil
}

// Add adds a favorite at the end of the user's list of its kind. It reports
// false, and changes nothing, when the favorite is already there, so that a
// repeated add is harmless. max caps the favorites of the user, of both
// kinds together.
func Add(ctx context.Context, db *sql.DB, userHK string, kind Kind, target string, max int) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("failed to start the add: %w", err)
	}
	defer tx.Rollback()
	if err := lockUser(ctx, tx, userHK); err != nil {
		return false, err
	}
	var exists bool
	var count, next int
	err = tx.QueryRowContext(ctx, `SELECT
			COALESCE(bool_or(kind = $2 AND target_hk = $3), FALSE),
			count(*),
			COALESCE(max(position) FILTER (WHERE kind = $2), -1) + 1
		FROM user_favorites WHERE user_hk = $1`, userHK, kind, target).Scan(&exists, &count, &next)
	if err != nil {
		return false, fmt.Errorf("failed to read the favorites: %w", err)
	}
	if exists {
		return false, nil
	}
	ok, err := listed(ctx, tx, kind, target)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ErrNotListed
	}
	if count >= max {
		return false, ErrLimit
	}
	var currency, market any
	if kind == KindCurrency {
		currency = target
	} else {
		market = target
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_favorites (user_hk, kind, target_hk, currency_hk, market_id, position)
		VALUES ($1, $2, $3, $4, $5, $6)`, userHK, kind, target, currency, market, next); err != nil {
		return false, fmt.Errorf("failed to add the favorite: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failed to add the favorite: %w", err)
	}
	return true, nil
}

// Remove removes a favorite. It reports false when there was none, so that a
// repeated remove is harmless.
func Remove(ctx context.Context, db *sql.DB, userHK string, kind Kind, target string) (bool, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM user_favorites WHERE user_hk = $1 AND kind = $2 AND target_hk = $3`, userHK, kind, target)
	if err != nil {
		return false, fmt.Errorf("failed to remove the favorite: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Reorder sets the order of the user's favorites of a kind. ordered must
// name each of them exactly once.
func Reorder(ctx context.Context, db *sql.DB, userHK string, kind Kind, ordered []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to start the reorder: %w", err)
	}
	defer tx.Rollback()
	if err := lockUser(ctx, tx, userHK); err != nil {
		return err
	}
	current, err := targets(ctx, tx, userHK, kind)
	if err != nil {
		return err
	}
	want := slices.Clone(ordered)
	slices.Sort(want)
	slices.Sort(current)
	if !slices.Equal(want, current) {
		return ErrOrderMismatch
	}
	for i, target := range ordered {
		if _, err := tx.ExecContext(ctx, `UPDATE user_favorites SET position = $4
			WHERE user_hk = $1 AND kind = $2 AND target_hk = $3`, userHK, kind, target, i); err != nil {
			return fmt.Errorf("failed to reorder the favorites: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to reorder the favorites: %w", err)
	}
	return nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func targets(ctx context.Context, q querier, userHK string, kind Kind) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT target_hk FROM user_favorites WHERE user_hk = $1 AND kind = $2 ORDER BY position, created_at`, userHK, kind)
	if err != nil {
		return nil, fmt.Errorf("failed to read the favorites: %w", err)
	}
	defer rows.Close()
	list := []string{}
	for rows.Next() {
		var hk string
		if err := rows.Scan(&hk); err != nil {
			return nil, fmt.Errorf("failed to read the favorites: %w", err)
		}
		list = append(list, hk)
	}
	return list, rows.Err()
}

// CurrencyHKs returns the hks of the user's favorite currencies, for the
// favorites filter of the currency lists.
func CurrencyHKs(ctx context.Context, db *sql.DB, userHK string) (map[string]bool, error) {
	list, err := targets(ctx, db, userHK, KindCurrency)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(list))
	for _, hk := range list {
		set[hk] = true
	}
	return set, nil
}

// Currency is a favorite currency, with its state now.
type Currency struct {
	HK     string `json:"hk" format:"uuid"`
	Symbol string `json:"symbol" example:"BTC"`
	EnName string `json:"en_name" example:"Bitcoin"`
	FaName string `json:"fa_name" example:"بیت کوین"`
	// Status is the currency's status: approved, or the suspend or deleted
	// that came after the user added it.
	Status  string    `json:"status" example:"approved"`
	AddedAt time.Time `json:"added_at"`
}

// Pair is a favorite trading pair, with its state now.
type Pair struct {
	ID     string `json:"id" format:"uuid"`
	Base   string `json:"base" example:"BTC"`
	Quote  string `json:"quote" example:"USDT"`
	Status string `json:"status" example:"ACTIVE"`
	// AddedAt is when the user added it.
	AddedAt time.Time `json:"added_at"`
}

// List is the favorites of a user, each kind in the user's order.
type List struct {
	Currencies []Currency `json:"currencies"`
	Pairs      []Pair     `json:"pairs"`
}

// Get returns the favorites of a user.
func Get(ctx context.Context, db *sql.DB, userHK string) (List, error) {
	list := List{Currencies: []Currency{}, Pairs: []Pair{}}
	rows, err := db.QueryContext(ctx, `
		SELECT c.hk, COALESCE(c.info->>'symbol', ''), COALESCE(c.info->>'en_name', ''), COALESCE(c.info->>'fa_name', ''),
			CASE WHEN c.deleted_at IS NOT NULL THEN 'deleted' ELSE c.status::TEXT END, f.created_at
		FROM user_favorites f JOIN currencies c ON c.hk = f.currency_hk
		WHERE f.user_hk = $1 AND f.kind = 'currency' ORDER BY f.position, f.created_at`, userHK)
	if err != nil {
		return List{}, fmt.Errorf("failed to read the favorites: %w", err)
	}
	for rows.Next() {
		var c Currency
		if err := rows.Scan(&c.HK, &c.Symbol, &c.EnName, &c.FaName, &c.Status, &c.AddedAt); err != nil {
			rows.Close()
			return List{}, fmt.Errorf("failed to read the favorites: %w", err)
		}
		list.Currencies = append(list.Currencies, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return List{}, fmt.Errorf("failed to read the favorites: %w", err)
	}

	rows, err = db.QueryContext(ctx, `
		SELECT m.id, b.info->>'symbol', q.info->>'symbol', m.status::TEXT, f.created_at
		FROM user_favorites f JOIN markets m ON m.id = f.market_id
		JOIN currencies b ON b.hk = m.base_currency_hk JOIN currencies q ON q.hk = m.quote_currency_hk
		WHERE f.user_hk = $1 AND f.kind = 'pair' ORDER BY f.position, f.created_at`, userHK)
	if err != nil {
		return List{}, fmt.Errorf("failed to read the favorites: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p Pair
		if err := rows.Scan(&p.ID, &p.Base, &p.Quote, &p.Status, &p.AddedAt); err != nil {
			return List{}, fmt.Errorf("failed to read the favorites: %w", err)
		}
		list.Pairs = append(list.Pairs, p)
	}
	return list, rows.Err()
}

// ValidTarget reports whether s is the UUID of a currency or pair.
func ValidTarget(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}
