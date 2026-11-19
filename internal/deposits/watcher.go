package deposits

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/big"
	"sort"
	"strconv"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// Block is a block of a network, with the transfers that it holds.
type Block struct {
	Number   int64
	ID       string
	ParentID string
	// Transfers are those of its successful transactions.
	Transfers []Transfer
}

// Transfer is a transfer of an asset in a transaction.
type Transfer struct {
	TxID string
	// Index is the index of the transfer's event in its transaction; -1
	// for a transfer of the native asset.
	Index int
	// Token is the contract of a token; "" for the native asset.
	Token    string
	From, To string
	// Amount is in the smallest unit of the asset.
	Amount *big.Int
}

// Chain reads a network: tron.Node for TRON.
type Chain interface {
	// Head returns the newest block, without its transfers.
	Head(ctx context.Context) (Block, error)
	// Block returns block number, with its transfers.
	Block(ctx context.Context, number int64) (Block, error)
	// TxBlock returns the block of a transaction in the canonical chain;
	// false for one that is not in it.
	TxBlock(ctx context.Context, txID string) (int64, bool, error)
}

// Watcher finds the deposits of a network to the deposit addresses, and
// credits them once their block is confirmed (#68). Every instance may run
// it: the cursor of the network, locked for each block, lets one record a
// block, and the next one finds it recorded.
type Watcher struct {
	DB      *sql.DB
	Network string
	Chain   Chain
	// Confirmations is how many blocks, the deposit's own included, make it
	// final: 19 on TRON, where a block is final once two thirds of the 27
	// producers have built on it.
	Confirmations int64
	// ReorgDepth is how far back the watcher reads again when a block does
	// not follow the one it read last.
	ReorgDepth int64
	// Start is the first block that a network without a cursor reads; 0
	// starts from the head.
	Start int64
	// MaxBlocks is the most blocks that one run reads.
	MaxBlocks int
	// Ignore are the addresses of the custody, such as the hot wallet: a
	// transfer from one, such as the fee of a sweep, is no deposit.
	Ignore map[string]bool
	// AML evaluates a deposit before it is credited; nil evaluates none.
	AML AML
}

// Result counts what a run did.
type Result struct {
	Blocks   int
	Detected int
	Credited int
	Held     int
	Failed   int
	Reorgs   int
}

type cursor struct {
	Number int64
	ID     string
}

var (
	errReorg = errors.New("deposits: the block does not follow the cursor")
	errMoved = errors.New("deposits: the cursor moved")
)

// Run reads the blocks after the cursor, up to the head and at most
// MaxBlocks, and then credits the deposits that are confirmed.
func (w *Watcher) Run(ctx context.Context) (Result, error) {
	var res Result
	head, err := w.Chain.Head(ctx)
	if err != nil {
		return res, err
	}
	cur, err := w.cursor(ctx, head)
	if err != nil {
		return res, err
	}
	for res.Blocks < w.MaxBlocks && cur.Number < head.Number {
		// Read outside the transaction: no lock waits on the node.
		b, err := w.Chain.Block(ctx, cur.Number+1)
		if err != nil {
			return res, err
		}
		next, err := w.apply(ctx, cur, b, &res)
		switch {
		case errors.Is(err, errReorg):
			res.Reorgs++
			if cur, err = w.rewind(ctx, cur); err != nil {
				return res, err
			}
		case errors.Is(err, errMoved):
			if cur, err = w.cursor(ctx, head); err != nil {
				return res, err
			}
		case err != nil:
			return res, err
		default:
			cur = next
			res.Blocks++
		}
	}
	err = w.confirm(ctx, head, &res)
	return res, err
}

// cursor reads the cursor of the network, checked against its seal; a
// network without one starts at Start, or at the head.
func (w *Watcher) cursor(ctx context.Context, head Block) (cursor, error) {
	c, err := readCursor(ctx, w.DB, w.Network, false)
	if !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}
	start := head
	if w.Start > 0 && w.Start <= head.Number {
		// The cursor is the block before the first one to read.
		if start, err = w.Chain.Block(ctx, w.Start-1); err != nil {
			return c, err
		}
	}
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return c, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO chain_cursors (network, block_number, block_id) VALUES ($1, $2, $3)
		ON CONFLICT (network) DO NOTHING`, w.Network, start.Number, start.ID); err != nil {
		return c, err
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.ChainCursors, "network = $1 AND row_seal IS NULL", w.Network); err != nil {
		return c, err
	}
	if err := tx.Commit(); err != nil {
		return c, err
	}
	log.Printf("deposits: the watcher of %s starts after block %d", w.Network, start.Number)
	return readCursor(ctx, w.DB, w.Network, false)
}

func readCursor(ctx context.Context, q querier, network string, lock bool) (cursor, error) {
	t := rowseal.ChainCursors
	query := `SELECT ` + t.Fields("") + ` FROM chain_cursors WHERE network = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	r := t.NewRow()
	if err := q.QueryRowContext(ctx, query, network).Scan(r.Dest()...); err != nil {
		return cursor{}, err
	}
	if err := rowseal.Require(ctx, t, r); err != nil {
		return cursor{}, err
	}
	n, err := strconv.ParseInt(r.Values[t.Index("block_number")].String, 10, 64)
	if err != nil {
		return cursor{}, err
	}
	return cursor{Number: n, ID: r.Values[t.Index("block_id")].String}, nil
}

func moveCursor(ctx context.Context, tx *sql.Tx, network string, to cursor) error {
	if _, err := tx.ExecContext(ctx, `UPDATE chain_cursors SET block_number = $2, block_id = $3, updated_at = NOW()
		WHERE network = $1`, network, to.Number, to.ID); err != nil {
		return err
	}
	return rowseal.Reseal(ctx, tx, rowseal.ChainCursors, "network = $1", network)
}

// rewind moves the cursor ReorgDepth blocks back, after a block that did
// not follow it: the blocks after it are read again, and a deposit found
// in another block is moved to it.
func (w *Watcher) rewind(ctx context.Context, from cursor) (cursor, error) {
	target := max(from.Number-max(w.ReorgDepth, 1), 0)
	b, err := w.Chain.Block(ctx, target)
	if err != nil {
		return from, err
	}
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return from, err
	}
	defer func() { _ = tx.Rollback() }()
	locked, err := readCursor(ctx, tx, w.Network, true)
	if err != nil {
		return from, err
	}
	if locked != from {
		return locked, tx.Commit()
	}
	to := cursor{Number: b.Number, ID: b.ID}
	if err := moveCursor(ctx, tx, w.Network, to); err != nil {
		return from, err
	}
	log.Printf("deposits: block %d of %s does not follow %s: reorganized, reading again from block %d",
		from.Number+1, w.Network, from.ID, to.Number+1)
	return to, tx.Commit()
}

// asset is a currency that the network takes deposits of.
type asset struct {
	currencyHK string
	decimals   int32
	min        decimal.Decimal
}

// assets returns the currencies of the network by contract, "" for the
// native one, each checked against its seal.
func assets(ctx context.Context, tx *sql.Tx, network string) (map[string]asset, error) {
	rows, err := tx.QueryContext(ctx, `SELECT currency_hk::text FROM currency_networks WHERE network = $1`, network)
	if err != nil {
		return nil, err
	}
	var hks []string
	for rows.Next() {
		var hk string
		if err := rows.Scan(&hk); err != nil {
			rows.Close()
			return nil, err
		}
		hks = append(hks, hk)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[string]asset{}
	for _, hk := range hks {
		n, err := currencies.NetworkOf(ctx, tx, hk, network, false)
		if err != nil {
			return nil, err
		}
		out[n.ContractAddress] = asset{currencyHK: hk, decimals: int32(n.Decimals), min: n.MinDeposit.Decimal()} //nolint:gosec // G115: decimals are 0 to 36 by the registry's check.
	}
	return out, nil
}

// found is a transfer to a deposit address.
type found struct {
	Transfer
	user  string
	asset asset
}

// apply records the deposits of block b, which must follow the cursor, and
// moves the cursor to it, in one transaction.
func (w *Watcher) apply(ctx context.Context, cur cursor, b Block, res *Result) (cursor, error) {
	tx, err := w.DB.BeginTx(ctx, nil)
	if err != nil {
		return cur, err
	}
	defer func() { _ = tx.Rollback() }()
	locked, err := readCursor(ctx, tx, w.Network, true)
	if err != nil {
		return cur, err
	}
	if locked != cur {
		return cur, errMoved
	}
	if b.Number != cur.Number+1 || b.ParentID != cur.ID {
		return cur, errReorg
	}
	byToken, err := assets(ctx, tx, w.Network)
	if err != nil {
		return cur, err
	}
	var hits []found
	for _, t := range b.Transfers {
		a, ok := byToken[t.Token]
		if !ok || w.Ignore[t.From] || t.Amount == nil || t.Amount.Sign() <= 0 {
			continue
		}
		user, ok, err := Owner(ctx, tx, w.Network, t.To)
		if err != nil {
			return cur, err
		}
		if ok {
			hits = append(hits, found{Transfer: t, user: user, asset: a})
		}
	}
	// The users' rows next, in one order, as every change of a user locks
	// them. Only the watcher locks the cursor, which it locks first.
	users := map[string]bool{}
	for _, h := range hits {
		users[h.user] = true
	}
	sorted := make([]string, 0, len(users))
	for u := range users {
		sorted = append(sorted, u)
	}
	sort.Strings(sorted)
	for _, u := range sorted {
		if err := restrictions.Lock(ctx, tx, u); err != nil {
			return cur, err
		}
	}
	for _, h := range hits {
		if err := w.record(ctx, tx, b, h, res); err != nil {
			return cur, fmt.Errorf("deposits: %s:%d: %w", h.TxID, h.Index, err)
		}
	}
	next := cursor{Number: b.Number, ID: b.ID}
	if err := moveCursor(ctx, tx, w.Network, next); err != nil {
		return cur, err
	}
	return next, tx.Commit()
}

// depositKey names the ledger entries of a deposit: ":detect" its first
// detection and ":credit" its credit, each once. A deposit that a reorg
// dropped and that came back, maybe in the very block it was first in, is
// detected and failed again with keys of their own; the deposit's row,
// locked in the transaction of each, lets each happen once.
func depositKey(network, txID string, index int) string {
	return "deposit:" + network + ":" + txID + ":" + strconv.Itoa(index)
}

func accountOf(userHK string) ledger.Account {
	id, _ := shared.UUIDFromString(userHK)
	return ledger.Account{UserHK: id}
}

func amountOf(currencyHK string, v shared.Decimal) ledger.Amount {
	id, _ := shared.UUIDFromString(currencyHK)
	return ledger.Amount{Currency: id, Value: v}
}

// record records a transfer of block b to a deposit address: a new
// deposit, DETECTED and pending in the user's balance, or FAILED below the
// minimum; or one seen before, which a reorg moved to b.
func (w *Watcher) record(ctx context.Context, tx *sql.Tx, b Block, h found, res *Result) error {
	amount := shared.Decimal(decimal.NewFromBigInt(h.Amount, -h.asset.decimals))
	seen, err := readDeposits(ctx, tx, `network = $1 AND tx_hash = $2 AND log_index = $3 FOR UPDATE`, w.Network, h.TxID, h.Index)
	if err != nil {
		return err
	}
	if len(seen) == 1 {
		d := seen[0]
		switch {
		case d.Status == StatusFailed && d.HoldReason == "":
			// Dropped by a reorg, and back in this block: pending again.
			if err := w.move(ctx, tx, d.ID, b, StatusDetected); err != nil {
				return err
			}
			res.Detected++
			_, err := ledger.Ledger{}.In(tx).DetectDeposit(ctx, depositKey(w.Network, h.TxID, h.Index)+":detect:"+uuid.NewString(),
				accountOf(d.UserHK), amountOf(d.CurrencyHK, d.Amount), ledger.Ref{Type: ledger.RefDeposit, ID: d.ID})
			return err
		case d.Status == StatusDetected && d.BlockHash != b.ID:
			return w.move(ctx, tx, d.ID, b, StatusDetected)
		}
		return nil
	}
	id := uuid.NewString()
	status, hold := StatusDetected, sql.NullString{}
	if h.asset.min.IsPositive() && amount.Decimal().LessThan(h.asset.min) {
		status, hold = StatusFailed, sql.NullString{String: string(HoldBelowMinimum), Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO deposits (id, tx_hash, currency_hk, address, amount, status, block_height,
			block_hash, network, log_index, user_id, from_address, hold_reason)
		VALUES ($1, $2, $3, $4, $5::numeric, $6, $7, $8, $9, $10, $11, NULLIF($12, ''), $13)`,
		id, h.TxID, h.asset.currencyHK, h.To, amount.String(), string(status), b.Number, b.ID, w.Network, h.Index, h.user,
		h.From, hold); err != nil {
		return err
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.Deposits, "id = $1", id); err != nil {
		return err
	}
	if status == StatusFailed {
		res.Failed++
		return nil
	}
	res.Detected++
	_, err = ledger.Ledger{}.In(tx).DetectDeposit(ctx, depositKey(w.Network, h.TxID, h.Index)+":detect",
		accountOf(h.user), amountOf(h.asset.currencyHK, amount), ledger.Ref{Type: ledger.RefDeposit, ID: id})
	return err
}

// move records a deposit in block b, with status.
func (w *Watcher) move(ctx context.Context, tx *sql.Tx, id string, b Block, status Status) error {
	if _, err := tx.ExecContext(ctx, `UPDATE deposits SET status = $2, block_height = $3, block_hash = $4 WHERE id = $1`,
		id, string(status), b.Number, b.ID); err != nil {
		return err
	}
	return rowseal.Reseal(ctx, tx, rowseal.Deposits, "id = $1", id)
}
