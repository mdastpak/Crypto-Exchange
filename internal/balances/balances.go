// Package balances reads a user's own balances and the history of their
// changes (#365), for GET /user/balances and GET /user/transactions.
//
// Every query filters by the user in SQL. A sub-account is the user's only
// if it is theirs and not deleted; another user's, an unknown one, and a
// deleted one are all ErrAccountNotFound, so the answer does not tell which
// exist. Every balance row read is checked against its row seal (#139): in
// enforce mode, one that fails is not returned, and the read fails with an
// *Unverified that names its currency; in report mode it is returned, and
// the finding is recorded either way. The history is append-only (#361);
// its seals (#366) do not exist yet.
//
// The history pages newest first, by the time of a change and its ID, both
// public: the ID is a UUID, never a serial. The internal fields of an entry
// stay internal: its idempotency key, its request hash, and its postings to
// the exchange's own accounts.
package balances

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/google/uuid"
)

// ErrAccountNotFound is a sub-account that is not the user's, does not
// exist, or is deleted.
var ErrAccountNotFound = errors.New("no such account")

// Unverified is a balance that fails its row seal in enforce mode: it is
// not returned. It wraps rowseal.ErrUnverified.
type Unverified struct {
	CurrencyHK string
	Err        error
}

func (e *Unverified) Error() string { return fmt.Sprintf("balance of %s: %v", e.CurrencyHK, e.Err) }

// Unwrap is the error of the seal.
func (e *Unverified) Unwrap() error { return e.Err }

// The values of the account selector.
const (
	AccountMain = "main"
	AccountAll  = "all"
)

// Account selects the accounts of a user: the main account, one
// sub-account, or every account.
type Account struct {
	All bool
	// Sub is the sub-account, or nil for the main account.
	Sub *uuid.UUID
}

// Kinds are the kinds of the history, the kinds of the ledger's entries.
var Kinds = []string{
	ledger.KindDepositDetected, ledger.KindDepositCredited, ledger.KindDepositFailed, ledger.KindLock,
	ledger.KindRelease, ledger.KindTransfer, ledger.KindTrade, ledger.KindWithdrawal, ledger.KindConversion,
}

// Querier reads the database.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store reads the balances and their history.
type Store struct {
	DB Querier
}

// Resolve reads the account selector raw of the user: "" or AccountMain,
// AccountAll, or the ID of a sub-account. It returns ErrAccountNotFound for
// a sub-account that is not the user's, unknown, or deleted, alike, and an
// error that is not for anything else that is not a selector.
func (s Store) Resolve(ctx context.Context, userHK, raw string) (Account, error) {
	switch raw {
	case "", AccountMain:
		return Account{}, nil
	case AccountAll:
		return Account{All: true}, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return Account{}, ErrAccountNotFound
	}
	var found bool
	if err := s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sub_accounts
		WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL)`, id, userHK).Scan(&found); err != nil {
		return Account{}, fmt.Errorf("read the sub-account: %w", err)
	}
	if !found {
		return Account{}, ErrAccountNotFound
	}
	return Account{Sub: &id}, nil
}

// filter returns the condition of a on the columns of alias, with its
// arguments numbered from next.
func (a Account) filter(alias string, next int) (string, []any) {
	switch {
	case a.All:
		// Every account of the user, but a deleted sub-account's.
		return fmt.Sprintf(`(%[1]s.sub_account_id IS NULL OR EXISTS (SELECT 1 FROM sub_accounts s
			WHERE s.id = %[1]s.sub_account_id AND s.deleted_at IS NULL))`, alias), nil
	case a.Sub != nil:
		return fmt.Sprintf("%s.sub_account_id = $%d", alias, next), []any{*a.Sub}
	}
	return alias + ".sub_account_id IS NULL", nil
}

// Balance is a balance of the user in one currency and account.
type Balance struct {
	CurrencyHK string `json:"currency_hk" format:"uuid"`
	Symbol     string `json:"symbol" example:"USDT"`
	// SubAccountID is the sub-account, absent for the main account.
	SubAccountID   *string        `json:"sub_account_id,omitempty" format:"uuid"`
	Available      shared.Decimal `json:"available" swaggertype:"string" example:"95.5"`
	Locked         shared.Decimal `json:"locked" swaggertype:"string" example:"4.5"`
	PendingDeposit shared.Decimal `json:"pending_deposit" swaggertype:"string" example:"0"`
	// Total is available plus locked.
	Total shared.Decimal `json:"total" swaggertype:"string" example:"100"`
	// Version rises with every change: a client that joins this snapshot
	// with the stream of changes applies only the events of a higher
	// version.
	Version int64 `json:"version" example:"7"`
}

// Balances returns the user's balances in the accounts of a, one per
// currency and account, optionally of one currency only. A currency with
// no balance row is left out.
func (s Store) Balances(ctx context.Context, userHK string, a Account, currencyHK string) ([]Balance, error) {
	where, args := a.filter("b", 2)
	query := `SELECT b.currency_hk::text, COALESCE(c.info->>'symbol', ''), b.sub_account_id::text,
			b.available, b.locked, b.pending_deposit, b.version, ` + rowseal.Balances.Fields("b") + `
		FROM balances b LEFT JOIN currencies c ON c.hk = b.currency_hk
		WHERE b.user_id = $1 AND ` + where
	args = append([]any{userHK}, args...)
	if currencyHK != "" {
		args = append(args, currencyHK)
		query += fmt.Sprintf(" AND b.currency_hk = $%d", len(args))
	}
	query += ` ORDER BY b.sub_account_id NULLS FIRST, c.info->>'symbol', b.currency_hk`
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read the balances: %w", err)
	}
	defer rows.Close()
	out := []Balance{}
	for rows.Next() {
		var b Balance
		var sub sql.NullString
		r := rowseal.Balances.NewRow()
		if err := rows.Scan(append([]any{&b.CurrencyHK, &b.Symbol, &sub, &b.Available, &b.Locked, &b.PendingDeposit, &b.Version},
			r.Dest()...)...); err != nil {
			return nil, fmt.Errorf("read the balances: %w", err)
		}
		// A balance changed outside the ledger is not shown in enforce
		// mode; its finding is recorded in both (#139).
		if err := rowseal.Check(ctx, rowseal.Balances, r); err != nil {
			return nil, &Unverified{CurrencyHK: b.CurrencyHK, Err: err}
		}
		if sub.Valid {
			b.SubAccountID = &sub.String
		}
		b.Total = shared.Decimal(b.Available.Decimal().Add(b.Locked.Decimal()))
		out = append(out, b)
	}
	return out, rows.Err()
}

// Change is one change of a balance of the user, as the history shows it.
type Change struct {
	// ID is the change, and EntryID the ledger entry it is part of: one
	// entry can change several balances, such as a trade.
	ID      string `json:"id" format:"uuid"`
	EntryID string `json:"entry_id" format:"uuid"`
	// Kind is the kind of the entry.
	Kind string `json:"kind" enums:"deposit_detected,deposit_credited,deposit_failed,lock,release,transfer,trade,withdrawal,conversion"`
	// ChangeType is what the change did to the balance.
	ChangeType string         `json:"change_type" example:"LOCK"`
	CurrencyHK string         `json:"currency_hk" format:"uuid"`
	Symbol     string         `json:"symbol" example:"USDT"`
	Amount     shared.Decimal `json:"amount" swaggertype:"string" example:"4.5"`
	// Available, Locked, and PendingDeposit are the balance after the
	// change.
	Available      shared.Decimal `json:"available" swaggertype:"string" example:"95.5"`
	Locked         shared.Decimal `json:"locked" swaggertype:"string" example:"4.5"`
	PendingDeposit shared.Decimal `json:"pending_deposit" swaggertype:"string" example:"0"`
	// SubAccountID is the sub-account, absent for the main account.
	SubAccountID *string `json:"sub_account_id,omitempty" format:"uuid"`
	// Reference is what caused the change, such as a withdrawal.
	Reference *Reference `json:"reference,omitempty"`
	Time      time.Time  `json:"time"`
}

// Reference is what caused a change.
type Reference struct {
	Type string `json:"type" example:"withdrawal"`
	ID   string `json:"id" example:"5f0c3c9e-2f43-4f7e-9a9c-2d6f1f5c1a10"`
}

// Filter selects the history.
type Filter struct {
	Account    Account
	CurrencyHK string
	Kind       string
	// Since and Until bound the time of the changes: since inclusive,
	// until exclusive. Zero is no bound.
	Since, Until time.Time
}

// Position is the place of a change in the history, newest first: the
// cursor of the next page.
type Position struct {
	Time time.Time `json:"t"`
	ID   string    `json:"id"`
}

// History returns at most limit changes of the user's balances that f
// selects, newest first, after position when it is set.
func (s Store) History(ctx context.Context, userHK string, f Filter, after *Position, limit int) ([]Change, error) {
	where, args := f.Account.filter("c", 2)
	conds := []string{"c.user_id = $1", where}
	args = append([]any{userHK}, args...)
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if f.CurrencyHK != "" {
		add("c.currency_hk = $%d", f.CurrencyHK)
	}
	if f.Kind != "" {
		add("e.kind = $%d", f.Kind)
	}
	if !f.Since.IsZero() {
		add(`c."timestamp" >= $%d`, f.Since)
	}
	if !f.Until.IsZero() {
		add(`c."timestamp" < $%d`, f.Until)
	}
	if after != nil {
		args = append(args, after.Time, after.ID)
		conds = append(conds, fmt.Sprintf(`(c."timestamp", c.id) < ($%d, $%d::uuid)`, len(args)-1, len(args)))
	}
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, `SELECT c.id::text, c.entry_id::text, e.kind, c.change_type, c.currency_hk::text,
			COALESCE(cur.info->>'symbol', ''), c.amount, c.new_available, c.new_locked, c.new_pending, c.sub_account_id::text,
			COALESCE(e.reference_type, ''), COALESCE(e.reference_id, ''), c."timestamp"
		FROM balance_changes c
		JOIN ledger_entries e ON e.id = c.entry_id
		LEFT JOIN currencies cur ON cur.hk = c.currency_hk
		WHERE `+strings.Join(conds, " AND ")+fmt.Sprintf(`
		ORDER BY c."timestamp" DESC, c.id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("read the history: %w", err)
	}
	defer rows.Close()
	out := []Change{}
	for rows.Next() {
		var ch Change
		var sub sql.NullString
		var refType, refID string
		if err := rows.Scan(&ch.ID, &ch.EntryID, &ch.Kind, &ch.ChangeType, &ch.CurrencyHK, &ch.Symbol, &ch.Amount,
			&ch.Available, &ch.Locked, &ch.PendingDeposit, &sub, &refType, &refID, &ch.Time); err != nil {
			return nil, fmt.Errorf("read the history: %w", err)
		}
		if sub.Valid {
			ch.SubAccountID = &sub.String
		}
		if refType != "" {
			ch.Reference = &Reference{Type: refType, ID: refID}
		}
		ch.Time = ch.Time.UTC()
		out = append(out, ch)
	}
	return out, rows.Err()
}
