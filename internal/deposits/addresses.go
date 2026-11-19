// Package deposits runs the deposits of crypto currencies (#68): the
// deposit address of each user on each network, derived from the extended
// public key of the custody, so the server holds no private key, and the
// deposits that the watcher of a network finds to them.
package deposits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// Deriver derives the address of an index of a network: tron's extended
// public key.
type Deriver interface {
	Derive(index uint32) (address, path string, err error)
}

// Levels checks that the user's level allows crypto deposits:
// verification.Store.
type Levels interface {
	Check(ctx context.Context, q verification.Querier, userHK string, a verification.Action) error
}

// Service gives the users their deposit addresses.
type Service struct {
	DB *sql.DB
	// Networks are the networks that take deposits, each with the
	// derivation of its addresses.
	Networks map[string]Deriver
	Levels   Levels
}

// Address is a deposit address of a user.
type Address struct {
	Network   string    `json:"network" example:"tron"`
	Address   string    `json:"address" example:"TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7"`
	CreatedAt time.Time `json:"created_at"`
}

// Errors of the addresses.
var (
	// ErrUnsupported is a network without deposits here.
	ErrUnsupported = errors.New("deposits: the network takes no deposits")
	// ErrClosed is a currency or a network whose deposits are closed.
	ErrClosed = errors.New("deposits: deposits of the currency on this network are closed")
	// ErrNotFound is a currency that is not listed.
	ErrNotFound = errors.New("deposits: no such currency")
)

// Get returns the user's deposit address of the network for the currency,
// derived and recorded at the first request. Every currency of a network
// shares the address. The user must be allowed crypto deposits, by level
// and restrictions, and the currency must take deposits on the network.
func (s Service) Get(ctx context.Context, userHK, currencyHK, network string, origin audit.Event) (Address, error) {
	derive, ok := s.Networks[network]
	if !ok {
		return Address{}, ErrUnsupported
	}
	if _, err := uuid.Parse(currencyHK); err != nil {
		return Address{}, ErrNotFound
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Address{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// The user's row first, as every change of a user locks it: a freeze
	// or a level that changes waits for this request, and the next one
	// sees it.
	if err := restrictions.Lock(ctx, tx, userHK); err != nil {
		return Address{}, err
	}
	if err := restrictions.Check(ctx, tx, userHK, restrictions.OpDeposit, currencyHK); err != nil {
		return Address{}, err
	}
	if err := s.Levels.Check(ctx, tx, userHK, verification.DepositCrypto); err != nil {
		return Address{}, err
	}
	cur, err := currencies.Detail(ctx, tx, currencyHK)
	if errors.Is(err, currencies.ErrNotFound) || (err == nil && (cur.DeletedAt != nil || cur.Status != models.CurrencyStatusApproved)) {
		return Address{}, ErrNotFound
	}
	if err != nil {
		return Address{}, err
	}
	if cur.Lifecycle != nil && !cur.Lifecycle.Capabilities.Deposit {
		return Address{}, ErrClosed
	}
	n, err := currencies.NetworkOf(ctx, tx, currencyHK, network, false)
	if errors.Is(err, currencies.ErrNetworkNotFound) {
		return Address{}, ErrUnsupported
	}
	if err != nil {
		return Address{}, err
	}
	if !n.DepositEnabled {
		return Address{}, ErrClosed
	}

	if a, err := current(ctx, tx, userHK, network); err == nil {
		return a, tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Address{}, err
	}
	// One index at a time per network, over every instance.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('deposit_addresses:' || $1))`, network); err != nil {
		return Address{}, err
	}
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(index) + 1, 0) FROM deposit_addresses WHERE network = $1`, network).
		Scan(&next); err != nil {
		return Address{}, err
	}
	if next < 0 || next > 1<<31-1 {
		return Address{}, fmt.Errorf("deposits: the addresses of %s are exhausted", network)
	}
	address, path, err := derive.Derive(uint32(next))
	if err != nil {
		return Address{}, err
	}
	id := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `INSERT INTO deposit_addresses (id, user_id, network, address, derivation_path, index)
		VALUES ($1, $2, $3, $4, $5, $6)`, id, userHK, network, address, path, next); err != nil {
		return Address{}, fmt.Errorf("deposits: record the address: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.DepositAddresses, "id = $1", id); err != nil {
		return Address{}, err
	}
	origin.ActorType, origin.ActorID = audit.ActorUser, userHK
	e := audit.Of(origin, audit.ActionUserDepositAddress, audit.TargetUser, userHK)
	e.Changes = &audit.Changes{After: map[string]any{"network": network, "address": address, "index": strconv.FormatInt(next, 10)}}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Address{}, err
	}
	a, err := current(ctx, tx, userHK, network)
	if err != nil {
		return Address{}, err
	}
	return a, tx.Commit()
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// current reads the user's active address of the network, checked against
// its seal; sql.ErrNoRows without one.
func current(ctx context.Context, q querier, userHK, network string) (Address, error) {
	t := rowseal.DepositAddresses
	r := t.NewRow()
	var a Address
	err := q.QueryRowContext(ctx, `SELECT `+t.Fields("")+` FROM deposit_addresses
		WHERE user_id = $1 AND network = $2 AND sub_account_id IS NULL AND active`, userHK, network).Scan(r.Dest()...)
	if err != nil {
		return a, err
	}
	return fromRow(ctx, r)
}

func fromRow(ctx context.Context, r *rowseal.Row) (Address, error) {
	t := rowseal.DepositAddresses
	if err := rowseal.Require(ctx, t, r); err != nil {
		return Address{}, err
	}
	created, err := time.Parse("2006-01-02T15:04:05.999999", r.Values[t.Index("created_at")].String)
	if err != nil {
		return Address{}, err
	}
	return Address{Network: r.Values[t.Index("network")].String, Address: r.Values[t.Index("address")].String, CreatedAt: created.UTC()}, nil
}

// List returns the user's deposit addresses, each checked against its seal.
func (s Service) List(ctx context.Context, userHK string) ([]Address, error) {
	t := rowseal.DepositAddresses
	//nolint:gosec // G202: the fields are the sealed columns of the table; the values are arguments.
	rows, err := s.DB.QueryContext(ctx, `SELECT `+t.Fields("")+` FROM deposit_addresses
		WHERE user_id = $1 AND sub_account_id IS NULL AND active ORDER BY network`, userHK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Address{}
	for rows.Next() {
		r := t.NewRow()
		if err := rows.Scan(r.Dest()...); err != nil {
			return nil, err
		}
		a, err := fromRow(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Owner returns the user of an address of the network, checked against
// its seal; ok is false for an address that is no user's. The watcher of a
// network credits a deposit to it.
func Owner(ctx context.Context, q querier, network, address string) (userHK string, ok bool, err error) {
	t := rowseal.DepositAddresses
	r := t.NewRow()
	err = q.QueryRowContext(ctx, `SELECT `+t.Fields("")+` FROM deposit_addresses WHERE network = $1 AND address = $2`,
		network, address).Scan(r.Dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if err := rowseal.Require(ctx, t, r); err != nil {
		return "", false, err
	}
	return r.Values[t.Index("user_id")].String, true, nil
}
