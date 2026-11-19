package deposits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"github.com/mdastpak/crypto-exchange/internal/aml"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// AML evaluates the rules of the AML engine for an operation:
// *aml.Engine.
type AML interface {
	Evaluate(ctx context.Context, tx *sql.Tx, ev aml.Event) (aml.Decision, error)
	Enforce(ctx context.Context, db *sql.DB, d aml.Decision, origin audit.Event) error
	Notify(ctx context.Context, notices []aml.Notice)
}

// MaxConfirmed is the most deposits that one run confirms.
const MaxConfirmed = 500

// confirm settles the deposits whose block has enough confirmations: one
// whose block is still canonical is credited or held; one whose
// transaction a reorg moved to another block waits for that block; one
// that a reorg dropped fails. Each deposit is settled in a transaction of
// its own, so that one that cannot be, such as a row changed in SQL, holds
// back no other.
func (w *Watcher) confirm(ctx context.Context, head Block, res *Result) error {
	final := head.Number - max(w.Confirmations, 1) + 1
	rows, err := w.DB.QueryContext(ctx, `SELECT id::text FROM deposits WHERE network = $1 AND status = 'DETECTED'
		AND block_height <= $2 ORDER BY block_height, id LIMIT $3`, w.Network, final, MaxConfirmed)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	blocks := map[int64]Block{}
	block := func(n int64) (Block, error) {
		if b, ok := blocks[n]; ok {
			return b, nil
		}
		b, err := w.Chain.Block(ctx, n)
		if err == nil {
			blocks[n] = b
		}
		return b, err
	}
	var failures []error
	for _, id := range ids {
		if err := w.settle(ctx, id, block, res); err != nil {
			log.Printf("deposits: deposit %s of %s not settled: %v", id, w.Network, err)
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("deposits: %d of %d confirmed deposits not settled: %w", len(failures), len(ids), errors.Join(failures...))
	}
	return nil
}

// settle settles the confirmed deposit id. The chain is read before the
// transaction, which then finds the deposit as it was read, or leaves it.
func (w *Watcher) settle(ctx context.Context, id string, block func(int64) (Block, error), res *Result) error {
	list, err := readDeposits(ctx, w.DB, `id = $1`, id)
	if err != nil || len(list) == 0 {
		return err
	}
	d := list[0]
	b, err := block(d.BlockHeight)
	if err != nil {
		return err
	}
	if b.ID == d.BlockHash {
		return w.credit(ctx, d, res)
	}
	n, ok, err := w.Chain.TxBlock(ctx, d.TxHash)
	if err != nil {
		return err
	}
	if ok {
		moved, err := block(n)
		if err != nil {
			return err
		}
		return w.inTx(ctx, d, func(tx *sql.Tx) error { return w.move(ctx, tx, d.ID, moved, StatusDetected) })
	}
	return w.inTx(ctx, d, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE deposits SET status = 'FAILED' WHERE id = $1`, d.ID); err != nil {
			return err
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.Deposits, "id = $1", d.ID); err != nil {
			return err
		}
		res.Failed++
		log.Printf("deposits: deposit %s (%s) of %s left the chain in a reorg: failed", d.ID, d.TxHash, w.Network)
		_, err := ledger.Ledger{}.In(tx).FailDeposit(ctx, depositKey(w.Network, d.TxHash, d.LogIndex)+":fail:"+uuid.NewString(),
			accountOf(d.UserHK), amountOf(d.CurrencyHK, d.Amount), ledger.Ref{Type: ledger.RefDeposit, ID: d.ID})
		return err
	})
}

// inTx runs f in a transaction that holds the user's row and the deposit,
// if the deposit is still DETECTED in the block that it was read in.
func (w *Watcher) inTx(ctx context.Context, d Deposit, f func(tx *sql.Tx) error) error {
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := restrictions.Lock(ctx, tx, d.UserHK); err != nil {
		return err
	}
	locked, err := lockDeposit(ctx, tx, d.ID)
	if err != nil {
		return err
	}
	if locked.Status != StatusDetected || locked.BlockHash != d.BlockHash {
		return nil
	}
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// credit credits a confirmed deposit, or holds it: when the deposits of
// its currency are closed, when the user's deposits are restricted, or
// when the AML rules refuse or review it. A refusal of the rules is
// recorded, and the account frozen when they ask, before the hold.
func (w *Watcher) credit(ctx context.Context, d Deposit, res *Result) error {
	var refused *aml.Decision
	var notices []aml.Notice
	err := w.inTx(ctx, d, func(tx *sql.Tx) error {
		hold, decision, err := w.check(ctx, tx, d)
		if err != nil {
			return err
		}
		if decision != nil && decision.Action.Refuses() {
			refused = decision
			return errRefused
		}
		if decision != nil {
			if notices, err = aml.Record(ctx, tx, *decision, audit.Event{ActorType: audit.ActorSystem}); err != nil {
				return err
			}
		}
		if hold != "" {
			res.Held++
			return holdIn(ctx, tx, d, hold)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE deposits SET status = 'CREDITED', confirmed_at = NOW(), credited_at = NOW()
			WHERE id = $1`, d.ID); err != nil {
			return err
		}
		if err := rowseal.Reseal(ctx, tx, rowseal.Deposits, "id = $1", d.ID); err != nil {
			return err
		}
		res.Credited++
		_, err = ledger.Ledger{}.In(tx).CreditDeposit(ctx, depositKey(w.Network, d.TxHash, d.LogIndex)+":credit",
			accountOf(d.UserHK), amountOf(d.CurrencyHK, d.Amount), ledger.Ref{Type: ledger.RefDeposit, ID: d.ID})
		return err
	})
	if errors.Is(err, errRefused) {
		// The transaction of the deposit rolled back: the refusal is
		// recorded on its own, and then the deposit held.
		if err := w.AML.Enforce(ctx, w.DB, *refused, audit.Event{ActorType: audit.ActorSystem}); !errors.Is(err, aml.ErrRefused) {
			return fmt.Errorf("record the AML refusal: %w", err)
		}
		return w.inTx(ctx, d, func(tx *sql.Tx) error {
			res.Held++
			return holdIn(ctx, tx, d, HoldAML)
		})
	}
	if err == nil && len(notices) > 0 && w.AML != nil {
		w.AML.Notify(ctx, notices)
	}
	return err
}

var errRefused = errors.New("deposits: refused by the AML rules")

// check tells why a confirmed deposit is held, if it is, and the decision
// of the AML rules, which it records in tx.
func (w *Watcher) check(ctx context.Context, tx *sql.Tx, d Deposit) (Hold, *aml.Decision, error) {
	cur, err := currencies.Detail(ctx, tx, d.CurrencyHK)
	if err != nil {
		return "", nil, err
	}
	if cur.DeletedAt != nil || cur.Status != models.CurrencyStatusApproved || (cur.Lifecycle != nil && !cur.Lifecycle.Capabilities.Deposit) {
		return HoldDepositsClosed, nil, nil
	}
	n, err := currencies.NetworkOf(ctx, tx, d.CurrencyHK, d.Network, false)
	if err != nil {
		return "", nil, err
	}
	if !n.DepositEnabled {
		return HoldDepositsClosed, nil, nil
	}
	var refusal *restrictions.Refusal
	if err := restrictions.Check(ctx, tx, d.UserHK, restrictions.OpDeposit, d.CurrencyHK); errors.As(err, &refusal) {
		return HoldRestricted, nil, nil
	} else if err != nil {
		return "", nil, err
	}
	if w.AML == nil {
		return "", nil, nil
	}
	decision, err := w.AML.Evaluate(ctx, tx, aml.Event{Kind: aml.KindDeposit, Ref: d.ID, UserHK: d.UserHK, CurrencyHK: d.CurrencyHK,
		Symbol: cur.Info.Symbol, Amount: d.Amount.Decimal(), Counterparty: d.FromAddress,
		CounterpartyAddress: d.FromAddress, CounterpartyNetwork: d.Network})
	if err != nil {
		return "", nil, err
	}
	if decision.Action == aml.ActionReview {
		return HoldAML, &decision, nil
	}
	return "", &decision, nil
}

// holdIn holds a confirmed deposit: it stays pending in the user's balance
// until an administrator credits it.
func holdIn(ctx context.Context, tx *sql.Tx, d Deposit, hold Hold) error {
	if _, err := tx.ExecContext(ctx, `UPDATE deposits SET status = 'CONFIRMED', confirmed_at = NOW(), hold_reason = $2
		WHERE id = $1`, d.ID, string(hold)); err != nil {
		return err
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.Deposits, "id = $1", d.ID); err != nil {
		return err
	}
	e := audit.Of(audit.Event{ActorType: audit.ActorSystem}, audit.ActionDepositHeld, audit.TargetDeposit, d.ID)
	e.Changes = &audit.Changes{After: map[string]any{"user_id": d.UserHK, "network": d.Network, "tx_hash": d.TxHash,
		"amount": d.Amount.String(), "hold_reason": string(hold)}}
	return audit.Record(ctx, tx, e)
}

// Errors of the credit of a held deposit.
var (
	// ErrNotHeld is a deposit that is not held: credited, pending, or
	// dropped by a reorg.
	ErrNotHeld = errors.New("deposits: the deposit is not on hold")
)

// Credit credits a held deposit, which an administrator reviewed: one
// below the minimum is first recorded as pending. It returns the deposit.
func Credit(ctx context.Context, db *sql.DB, id string, origin audit.Event) (Deposit, error) {
	list, err := readDeposits(ctx, db, `id = $1`, id)
	if err != nil {
		return Deposit{}, err
	}
	if len(list) == 0 {
		return Deposit{}, ErrDepositNotFound
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Deposit{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := restrictions.Lock(ctx, tx, list[0].UserHK); err != nil {
		return Deposit{}, err
	}
	d, err := lockDeposit(ctx, tx, id)
	if err != nil {
		return Deposit{}, err
	}
	held := d.Status == StatusConfirmed || (d.Status == StatusFailed && d.HoldReason == HoldBelowMinimum)
	if d.HoldReason == "" || !held {
		return Deposit{}, ErrNotHeld
	}
	key := depositKey(d.Network, d.TxHash, d.LogIndex)
	ref := ledger.Ref{Type: ledger.RefDeposit, ID: d.ID}
	l := ledger.Ledger{}.In(tx)
	if d.Status == StatusFailed {
		if _, err := l.DetectDeposit(ctx, key+":detect", accountOf(d.UserHK), amountOf(d.CurrencyHK, d.Amount), ref); err != nil {
			return Deposit{}, err
		}
	}
	if _, err := l.CreditDeposit(ctx, key+":credit", accountOf(d.UserHK), amountOf(d.CurrencyHK, d.Amount), ref); err != nil {
		return Deposit{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE deposits SET status = 'CREDITED', hold_reason = NULL,
		confirmed_at = COALESCE(confirmed_at, NOW()), credited_at = NOW() WHERE id = $1`, d.ID); err != nil {
		return Deposit{}, err
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.Deposits, "id = $1", d.ID); err != nil {
		return Deposit{}, err
	}
	e := audit.Of(origin, audit.ActionAdminDepositCredit, audit.TargetDeposit, d.ID)
	e.Changes = &audit.Changes{
		Before: map[string]any{"status": string(d.Status), "hold_reason": string(d.HoldReason)},
		After:  map[string]any{"status": string(StatusCredited), "user_id": d.UserHK, "amount": d.Amount.String()},
	}
	if err := audit.Record(ctx, tx, e); err != nil {
		return Deposit{}, err
	}
	credited, err := lockDeposit(ctx, tx, id)
	if err != nil {
		return Deposit{}, err
	}
	return credited, tx.Commit()
}
