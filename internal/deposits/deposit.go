package deposits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// Status is the state of a deposit.
type Status string

// The states of a deposit. A deposit is DETECTED in its block and CREDITED
// once the block is confirmed and still canonical; CONFIRMED is one that
// is confirmed and held, and FAILED one that a reorg dropped, or that is
// below the minimum.
const (
	StatusDetected  Status = "DETECTED"
	StatusConfirmed Status = "CONFIRMED"
	StatusCredited  Status = "CREDITED"
	StatusFailed    Status = "FAILED"
)

// Hold is why a deposit is not credited.
type Hold string

// The holds of a deposit. An administrator may credit a held deposit after
// review.
const (
	HoldBelowMinimum   Hold = "below_minimum"
	HoldDepositsClosed Hold = "deposits_closed"
	HoldRestricted     Hold = "restricted"
	HoldAML            Hold = "aml"
)

// Deposit is a deposit on chain, as an administrator sees it.
type Deposit struct {
	ID         string `json:"id"`
	UserHK     string `json:"user_id"`
	CurrencyHK string `json:"currency_hk"`
	Network    string `json:"network" example:"tron"`
	TxHash     string `json:"tx_hash"`
	// LogIndex is the index of the transfer's event in its transaction; -1
	// for a transfer of the native asset.
	LogIndex    int            `json:"log_index"`
	Address     string         `json:"address"`
	FromAddress string         `json:"from_address,omitempty"`
	Amount      shared.Decimal `json:"amount" swaggertype:"string" example:"25.5"`
	Status      Status         `json:"status" example:"CREDITED"`
	BlockHeight int64          `json:"block_height"`
	BlockHash   string         `json:"block_hash"`
	HoldReason  Hold           `json:"hold_reason,omitempty"`
	DetectedAt  time.Time      `json:"detected_at"`
	CreditedAt  *time.Time     `json:"credited_at,omitempty"`
}

// UserDeposit is a deposit as its user sees it: on hold or not, without
// why, as an AML hold must not be told.
type UserDeposit struct {
	ID          string         `json:"id"`
	CurrencyHK  string         `json:"currency_hk"`
	Network     string         `json:"network" example:"tron"`
	TxHash      string         `json:"tx_hash"`
	Address     string         `json:"address"`
	Amount      shared.Decimal `json:"amount" swaggertype:"string" example:"25.5"`
	Status      Status         `json:"status" example:"CREDITED"`
	OnHold      bool           `json:"on_hold"`
	BlockHeight int64          `json:"block_height"`
	DetectedAt  time.Time      `json:"detected_at"`
	CreditedAt  *time.Time     `json:"credited_at,omitempty"`
}

// ForUser is d as its user sees it. A deposit below the minimum is not
// shown as on hold: it is not credited unless support credits it.
func (d Deposit) ForUser() UserDeposit {
	return UserDeposit{ID: d.ID, CurrencyHK: d.CurrencyHK, Network: d.Network, TxHash: d.TxHash, Address: d.Address,
		Amount: d.Amount, Status: d.Status, OnHold: d.Status == StatusConfirmed && d.HoldReason != "",
		BlockHeight: d.BlockHeight, DetectedAt: d.DetectedAt, CreditedAt: d.CreditedAt}
}

const timeLayout = "2006-01-02T15:04:05.999999"

// depositFrom reads a deposit from its sealed row, checked against its
// seal.
func depositFrom(ctx context.Context, r *rowseal.Row) (Deposit, error) {
	t := rowseal.Deposits
	if err := rowseal.Require(ctx, t, r); err != nil {
		return Deposit{}, err
	}
	v := func(name string) string { return r.Values[t.Index(name)].String }
	d := Deposit{ID: r.Key, UserHK: v("user_id"), CurrencyHK: v("currency_hk"), Network: v("network"), TxHash: v("tx_hash"),
		Address: v("address"), FromAddress: v("from_address"), Status: Status(v("status")), BlockHash: v("block_hash"),
		HoldReason: Hold(v("hold_reason"))}
	var err error
	if d.LogIndex, err = strconv.Atoi(v("log_index")); err != nil {
		return d, fmt.Errorf("deposits: log index of %s: %w", d.ID, err)
	}
	if d.BlockHeight, err = strconv.ParseInt(v("block_height"), 10, 64); err != nil {
		return d, fmt.Errorf("deposits: block of %s: %w", d.ID, err)
	}
	if d.Amount, err = shared.NewDecimalFromString(v("amount")); err != nil {
		return d, fmt.Errorf("deposits: amount of %s: %w", d.ID, err)
	}
	if d.DetectedAt, err = time.Parse(timeLayout, v("detected_at")); err != nil {
		return d, err
	}
	d.DetectedAt = d.DetectedAt.UTC()
	if c := r.Values[t.Index("credited_at")]; c.Valid {
		at, err := time.Parse(timeLayout, c.String)
		if err != nil {
			return d, err
		}
		at = at.UTC()
		d.CreditedAt = &at
	}
	return d, nil
}

// readDeposits reads the deposits that match where, each checked against
// its seal.
func readDeposits(ctx context.Context, q querier, where string, args ...any) ([]Deposit, error) {
	t := rowseal.Deposits
	rows, err := q.QueryContext(ctx, `SELECT `+t.Fields("")+` FROM deposits WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Deposit{}
	for rows.Next() {
		r := t.NewRow()
		if err := rows.Scan(r.Dest()...); err != nil {
			return nil, err
		}
		d, err := depositFrom(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ErrDepositNotFound is a deposit that does not exist.
var ErrDepositNotFound = errors.New("deposits: no such deposit")

// lockDeposit reads a deposit for update, checked against its seal.
func lockDeposit(ctx context.Context, tx *sql.Tx, id string) (Deposit, error) {
	list, err := readDeposits(ctx, tx, `id = $1 FOR UPDATE`, id)
	if err != nil {
		return Deposit{}, err
	}
	if len(list) == 0 {
		return Deposit{}, ErrDepositNotFound
	}
	return list[0], nil
}

// MaxListed is the most deposits that a list returns.
const MaxListed = 200

// UserDeposits returns the user's newest deposits.
func UserDeposits(ctx context.Context, db *sql.DB, userHK string) ([]UserDeposit, error) {
	list, err := readDeposits(ctx, db, `user_id = $1 ORDER BY detected_at DESC, id LIMIT `+strconv.Itoa(MaxListed), userHK)
	if err != nil {
		return nil, err
	}
	out := make([]UserDeposit, len(list))
	for i, d := range list {
		out[i] = d.ForUser()
	}
	return out, nil
}

// Filter narrows the deposits that an administrator lists.
type Filter struct {
	UserHK string
	Status Status
	// Held lists only the deposits on hold.
	Held bool
}

// List returns the newest deposits that match f, for an administrator.
func List(ctx context.Context, db *sql.DB, f Filter) ([]Deposit, error) {
	return readDeposits(ctx, db, `($1 = '' OR user_id::text = $1) AND ($2 = '' OR status::text = $2)
		AND (NOT $3 OR hold_reason IS NOT NULL) ORDER BY detected_at DESC, id LIMIT `+strconv.Itoa(MaxListed),
		f.UserHK, string(f.Status), f.Held)
}

// History returns the user's newest deposits.
func (s Service) History(ctx context.Context, userHK string) ([]UserDeposit, error) {
	return UserDeposits(ctx, s.DB, userHK)
}

// Reviews are the deposits as administrators review them.
type Reviews struct {
	DB *sql.DB
}

// List is List on the database.
func (r Reviews) List(ctx context.Context, f Filter) ([]Deposit, error) { return List(ctx, r.DB, f) }

// Credit is Credit on the database.
func (r Reviews) Credit(ctx context.Context, id string, origin audit.Event) (Deposit, error) {
	return Credit(ctx, r.DB, id, origin)
}
