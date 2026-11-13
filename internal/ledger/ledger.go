// Package ledger keeps the books of the exchange (#60): the balances of
// users, and the exchange's own accounts, in double entry.
//
// Every operation, such as a credited deposit, a lock for an order, or a
// settled trade, is one ledger entry, written in one transaction:
//
//   - the changes of the balances of users, each a transition of the balance
//     state machine (internal/domain/balance), recorded in balance_changes
//     with the components before and after and the version it made;
//   - the postings to the exchange's own accounts, in ledger_postings:
//     custody, fees, inventory, and deposits pending.
//
// A user's position in a currency is available + locked + pending. In each
// entry and currency, the changes of the positions plus the postings sum to
// zero: a constraint trigger refuses an entry that does not, at commit. So
// the exchange's accounts always mirror what users hold:
//
//	users' positions + fees + inventory + deposits pending = -custody
//
// where -custody is what custody must hold, which the reconciliation
// compares with the holdings that custody reports.
//
// Every operation takes an idempotency key. The same key with the same
// request applies once and returns the same entry when it is retried; with
// another request it is refused. The balances of an entry are locked in a
// fixed order for the transaction, and every change raises the version of
// its balance, so concurrent operations never lose an update or drive a
// balance below zero. There is no public write API: the services of the
// exchange, such as OTC execution, settlement, and custody, call this
// package.
package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/balance"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/metrics"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/jackc/pgx/v5/pgconn"
)

// The exchange's own accounts, one of each per currency.
const (
	// AccountCustody holds the negative of what custody must hold for the
	// currency: every credited deposit lowers it, every completed
	// withdrawal raises it.
	AccountCustody = "custody"
	// AccountFees holds the fees the exchange earned.
	AccountFees = "fees"
	// AccountInventory holds what the exchange trades with as the
	// counterparty of users, in OTC trades.
	AccountInventory = "inventory"
	// AccountDepositsPending holds the negative of the deposits detected
	// and not credited yet.
	AccountDepositsPending = "deposits_pending"
)

// Accounts lists the exchange's accounts, in a fixed order.
var Accounts = []string{AccountCustody, AccountFees, AccountInventory, AccountDepositsPending}

// The kinds of entries.
const (
	KindDepositDetected = "deposit_detected"
	KindDepositCredited = "deposit_credited"
	KindDepositFailed   = "deposit_failed"
	KindLock            = "lock"
	KindRelease         = "release"
	KindTransfer        = "transfer"
	KindTrade           = "trade"
	KindWithdrawal      = "withdrawal"
)

var (
	// ErrIdempotencyConflict refuses an operation whose idempotency key
	// was used for another request.
	ErrIdempotencyConflict = errors.New("the idempotency key was used for another request")
	// ErrInvalidRequest refuses an operation that is malformed, such as one
	// of an amount that is not positive, or of an unknown currency.
	ErrInvalidRequest = errors.New("invalid ledger request")
)

// The errors of balances that an operation would drive below zero, from
// the state machine.
var (
	ErrInsufficientBalance = balance.ErrInsufficientBalance
	ErrInsufficientLocked  = balance.ErrInsufficientLocked
	ErrInsufficientPending = balance.ErrInsufficientPending
)

// ErrUnverified refuses an operation on a balance that fails its row seal
// in enforce mode (#139): it was changed outside the ledger.
var ErrUnverified = rowseal.ErrUnverified

// Ledger keeps the books in a PostgreSQL database.
type Ledger struct {
	DB *sql.DB
}

// Account names the owner of a balance: a user, and a sub-account of the
// user or none.
type Account struct {
	UserHK       shared.UUID
	SubAccountID *shared.UUID
}

func (a Account) String() string {
	if a.SubAccountID == nil {
		return a.UserHK.String()
	}
	return a.UserHK.String() + "/" + a.SubAccountID.String()
}

// Ref names what caused an entry, such as "order" and its ID, for the
// records. Both are empty, or both are set.
type Ref struct {
	Type, ID string
}

// Amount is an amount of a currency.
type Amount struct {
	Currency shared.UUID
	Value    shared.Decimal
}

// Result is the outcome of an operation.
type Result struct {
	// EntryID is the ledger entry of the operation.
	EntryID string
	// Replayed reports an operation whose key was applied before, with the
	// same request: nothing changed now, and EntryID is that of the first.
	Replayed bool
}

// move is one transition of a balance of an entry.
type move struct {
	account  Account
	currency shared.UUID
	change   balance.BalanceChangeType
	amount   shared.Decimal
}

// posting is one posting of an entry to an account of the exchange; the
// amount is signed.
type posting struct {
	account  string
	currency shared.UUID
	amount   shared.Decimal
}

// entry is an operation, before it is recorded.
type entry struct {
	kind     string
	key      string
	ref      Ref
	moves    []move
	postings []posting
}

// DetectDeposit records a deposit to a that is detected on its network but
// not credited yet: pending, not owned.
func (l Ledger) DetectDeposit(ctx context.Context, key string, a Account, amount Amount, ref Ref) (Result, error) {
	return l.record(ctx, entry{kind: KindDepositDetected, key: key, ref: ref,
		moves:    []move{{a, amount.Currency, balance.BalanceChangePendingDep, amount.Value}},
		postings: []posting{{AccountDepositsPending, amount.Currency, amount.Value.Neg()}},
	})
}

// CreditDeposit credits a detected deposit to a: the pending amount becomes
// available, and custody now holds it.
func (l Ledger) CreditDeposit(ctx context.Context, key string, a Account, amount Amount, ref Ref) (Result, error) {
	return l.record(ctx, entry{kind: KindDepositCredited, key: key, ref: ref,
		moves: []move{{a, amount.Currency, balance.BalanceChangeDeposit, amount.Value}},
		postings: []posting{
			{AccountDepositsPending, amount.Currency, amount.Value},
			{AccountCustody, amount.Currency, amount.Value.Neg()},
		},
	})
}

// FailDeposit drops a detected deposit that will not be credited.
func (l Ledger) FailDeposit(ctx context.Context, key string, a Account, amount Amount, ref Ref) (Result, error) {
	return l.record(ctx, entry{kind: KindDepositFailed, key: key, ref: ref,
		moves:    []move{{a, amount.Currency, balance.BalanceChangeDepositFailed, amount.Value}},
		postings: []posting{{AccountDepositsPending, amount.Currency, amount.Value}},
	})
}

// Lock reserves available funds of a for an order or a withdrawal.
func (l Ledger) Lock(ctx context.Context, key string, a Account, amount Amount, ref Ref) (Result, error) {
	return l.record(ctx, entry{kind: KindLock, key: key, ref: ref,
		moves: []move{{a, amount.Currency, balance.BalanceChangeLock, amount.Value}},
	})
}

// Release makes locked funds of a available again, when an order is
// cancelled or a withdrawal fails.
func (l Ledger) Release(ctx context.Context, key string, a Account, amount Amount, ref Ref) (Result, error) {
	return l.record(ctx, entry{kind: KindRelease, key: key, ref: ref,
		moves: []move{{a, amount.Currency, balance.BalanceChangeUnlock, amount.Value}},
	})
}

// Transfer moves available funds from one account to another: they leave
// from, through its lock, and arrive available in to.
func (l Ledger) Transfer(ctx context.Context, key string, from, to Account, amount Amount, ref Ref) (Result, error) {
	if from.String() == to.String() {
		return Result{}, fmt.Errorf("%w: a transfer to the same account", ErrInvalidRequest)
	}
	return l.record(ctx, entry{kind: KindTransfer, key: key, ref: ref,
		moves: []move{
			{from, amount.Currency, balance.BalanceChangeLock, amount.Value},
			{from, amount.Currency, balance.BalanceChangeTransferOut, amount.Value},
			{to, amount.Currency, balance.BalanceChangeTransferIn, amount.Value},
		},
	})
}

// Trade is a trade of a user with the exchange as the counterparty, as in
// OTC: the user gives Give, from funds locked with the order, receives
// Receive, available, and pays Fee, also from the locked funds. A Fee of
// zero is none.
type Trade struct {
	Give, Receive, Fee Amount
}

// SettleTrade settles a trade of a against the exchange's inventory: what
// the user gives enters the inventory, what the user receives leaves it,
// and the fee goes to the fees account.
func (l Ledger) SettleTrade(ctx context.Context, key string, a Account, t Trade, ref Ref) (Result, error) {
	if t.Give.Currency == t.Receive.Currency {
		return Result{}, fmt.Errorf("%w: a trade gives and receives %s", ErrInvalidRequest, t.Give.Currency)
	}
	e := entry{kind: KindTrade, key: key, ref: ref,
		moves: []move{
			{a, t.Give.Currency, balance.BalanceChangeTradeDebit, t.Give.Value},
			{a, t.Receive.Currency, balance.BalanceChangeTradeCredit, t.Receive.Value},
		},
		postings: []posting{
			{AccountInventory, t.Give.Currency, t.Give.Value},
			{AccountInventory, t.Receive.Currency, t.Receive.Value.Neg()},
		},
	}
	if !t.Fee.Value.IsZero() {
		e.moves = append(e.moves, move{a, t.Fee.Currency, balance.BalanceChangeFee, t.Fee.Value})
		e.postings = append(e.postings, posting{AccountFees, t.Fee.Currency, t.Fee.Value})
	}
	return l.record(ctx, e)
}

// Withdraw completes a withdrawal of a, whose amount and fee were locked
// when it was requested: the amount leaves custody, and the fee goes to the
// fees account. A fee of zero is none.
func (l Ledger) Withdraw(ctx context.Context, key string, a Account, amount Amount, fee shared.Decimal, ref Ref) (Result, error) {
	e := entry{kind: KindWithdrawal, key: key, ref: ref,
		moves:    []move{{a, amount.Currency, balance.BalanceChangeWithdrawal, amount.Value}},
		postings: []posting{{AccountCustody, amount.Currency, amount.Value}},
	}
	if !fee.IsZero() {
		e.moves = append(e.moves, move{a, amount.Currency, balance.BalanceChangeFee, fee})
		e.postings = append(e.postings, posting{AccountFees, amount.Currency, fee})
	}
	return l.record(ctx, e)
}

// validate checks an entry before it touches the database.
func (e entry) validate() error {
	if e.key == "" || len(e.key) > 200 {
		return fmt.Errorf("%w: the idempotency key must have 1 to 200 characters", ErrInvalidRequest)
	}
	if (e.ref.Type == "") != (e.ref.ID == "") {
		return fmt.Errorf("%w: a reference has a type and an ID", ErrInvalidRequest)
	}
	for _, m := range e.moves {
		if !m.amount.IsPositive() {
			return fmt.Errorf("%w: %s of %s must be positive", ErrInvalidRequest, m.change, m.amount)
		}
		if m.account.UserHK.IsZero() || m.currency.IsZero() {
			return fmt.Errorf("%w: a balance names its user and its currency", ErrInvalidRequest)
		}
	}
	for _, p := range e.postings {
		if p.amount.IsZero() {
			return fmt.Errorf("%w: a posting to %s of zero", ErrInvalidRequest, p.account)
		}
	}
	return nil
}

// hash is the SHA-256 of what the entry does, which a retry of its key
// must match. Amounts are written in their shortest form, so that 1.50 and
// 1.5 are the same request.
func (e entry) hash() []byte {
	var b strings.Builder
	field := func(s string) {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	field(e.kind)
	field(e.ref.Type)
	field(e.ref.ID)
	for _, m := range e.moves {
		field("move")
		field(m.account.String())
		field(m.currency.String())
		field(string(m.change))
		field(m.amount.Decimal().String())
	}
	for _, p := range e.postings {
		field("posting")
		field(p.account)
		field(p.currency.String())
		field(p.amount.Decimal().String())
	}
	sum := sha256.Sum256([]byte(b.String()))
	return sum[:]
}

// balanceKey identifies a balance: its account and currency.
type balanceKey struct {
	user, sub, currency string
}

func keyOf(a Account, currency shared.UUID) balanceKey {
	k := balanceKey{user: a.UserHK.String(), currency: currency.String()}
	if a.SubAccountID != nil {
		k.sub = a.SubAccountID.String()
	}
	return k
}

// record applies an entry, and counts and times it in the metrics.
func (l Ledger) record(ctx context.Context, e entry) (Result, error) {
	start := time.Now()
	res, err := l.apply(ctx, e)
	observe(e.kind, start, res, err)
	return res, err
}

// observe counts and times an operation of kind that started at start.
func observe(kind string, start time.Time, res Result, err error) {
	metrics.LedgerOperations.WithLabelValues(kind, outcome(res, err)).Inc()
	metrics.LedgerOperationDuration.WithLabelValues(kind).Observe(time.Since(start).Seconds())
}

// outcome names the outcome of an operation for the metrics.
func outcome(res Result, err error) string {
	switch {
	case err == nil && res.Replayed:
		return "replayed"
	case err == nil:
		return "applied"
	case errors.Is(err, ErrInsufficientBalance), errors.Is(err, ErrInsufficientLocked), errors.Is(err, ErrInsufficientPending):
		return "insufficient"
	case errors.Is(err, ErrInvalidRequest):
		return "invalid"
	case errors.Is(err, ErrIdempotencyConflict):
		return "conflict"
	case errors.Is(err, ErrUnverified):
		return "unverified"
	}
	return "failed"
}

// apply applies an entry in one transaction; see the package comment.
func (l Ledger) apply(ctx context.Context, e entry) (Result, error) {
	if err := e.validate(); err != nil {
		return Result{}, err
	}
	tx, err := l.DB.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, err
	}
	defer tx.Rollback()
	res, err := write(ctx, tx, e)
	if err != nil || res.Replayed {
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, mapError(fmt.Errorf("commit the entry: %w", err))
	}
	return res, nil
}

// write writes an entry in tx, which the caller commits.
func write(ctx context.Context, tx *sql.Tx, e entry) (Result, error) {
	hash := e.hash()
	// The key first: a concurrent operation with the same key waits here
	// for the first to commit, and then finds its entry.
	var entryID string
	err := tx.QueryRowContext(ctx, `INSERT INTO ledger_entries (kind, idempotency_key, request_hash, reference_type, reference_id)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''))
		ON CONFLICT (idempotency_key) DO NOTHING RETURNING id`, e.kind, e.key, hash, e.ref.Type, e.ref.ID).Scan(&entryID)
	if errors.Is(err, sql.ErrNoRows) {
		var stored []byte
		if err := tx.QueryRowContext(ctx, `SELECT id, request_hash FROM ledger_entries WHERE idempotency_key = $1`, e.key).
			Scan(&entryID, &stored); err != nil {
			return Result{}, fmt.Errorf("read the entry of key %q: %w", e.key, err)
		}
		if !bytes.Equal(stored, hash) {
			return Result{}, fmt.Errorf("%w: %q", ErrIdempotencyConflict, e.key)
		}
		return Result{EntryID: entryID, Replayed: true}, nil
	}
	if err != nil {
		return Result{}, mapError(fmt.Errorf("record the entry: %w", err))
	}

	balances, err := lockBalances(ctx, tx, e.moves)
	if err != nil {
		return Result{}, err
	}
	read := make(map[balanceKey]int, len(balances))
	for k, b := range balances {
		read[k] = b.Version
	}
	var events []balance.BalanceChangeEvent
	for _, m := range e.moves {
		b := balances[keyOf(m.account, m.currency)]
		event, err := b.Apply(m.change, m.amount, balance.Reference{})
		if err != nil {
			return Result{}, fmt.Errorf("%s of %s in %s for %s: %w", m.change, m.amount, m.currency, m.account, err)
		}
		events = append(events, event)
	}
	var written []string
	for k, b := range balances {
		res, err := tx.ExecContext(ctx, `UPDATE balances SET available = $2, locked = $3, pending_deposit = $4, version = $5
			WHERE id = $1 AND version = $6`, b.ID, b.Available, b.Locked, b.PendingDeposit, b.Version, read[k])
		if err != nil {
			return Result{}, mapError(fmt.Errorf("write the balance: %w", err))
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return Result{}, fmt.Errorf("write balance %s: it changed under its lock", b.ID)
		}
		written = append(written, b.ID.String())
	}
	// The balances are sealed again with what the entry wrote (#139).
	if err := rowseal.Reseal(ctx, tx, rowseal.Balances, "id = ANY($1::uuid[])", written); err != nil {
		return Result{}, err
	}
	for _, ev := range events {
		if _, err := tx.ExecContext(ctx, `INSERT INTO balance_changes (entry_id, version, balance_id, user_id, sub_account_id,
				currency_hk, change_type, amount, old_available, new_available, old_locked, new_locked, old_pending, new_pending)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			entryID, ev.NewVersion, ev.BalanceID, ev.UserID, ev.SubAccountID, ev.CurrencyHK, string(ev.ChangeType), ev.Amount,
			ev.OldAvailable, ev.NewAvailable, ev.OldLocked, ev.NewLocked, ev.OldPending, ev.NewPending); err != nil {
			return Result{}, mapError(fmt.Errorf("record the change: %w", err))
		}
	}
	for _, p := range e.postings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_postings (entry_id, account, currency_hk, amount) VALUES ($1, $2, $3, $4)`,
			entryID, p.account, p.currency, p.amount); err != nil {
			return Result{}, mapError(fmt.Errorf("record the posting: %w", err))
		}
	}
	return Result{EntryID: entryID}, nil
}

// lockBalances returns the balances of the moves, locked for the
// transaction, creating the ones that do not exist yet. They are created
// and locked in a fixed order, so that two entries over the same balances
// wait for each other instead of deadlocking.
func lockBalances(ctx context.Context, tx *sql.Tx, moves []move) (map[balanceKey]*balance.Balance, error) {
	type wanted struct {
		key      balanceKey
		account  Account
		currency shared.UUID
	}
	seen := make(map[balanceKey]bool)
	var all []wanted
	for _, m := range moves {
		k := keyOf(m.account, m.currency)
		if !seen[k] {
			seen[k] = true
			all = append(all, wanted{k, m.account, m.currency})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i].key, all[j].key
		if a.user != b.user {
			return a.user < b.user
		}
		if a.sub != b.sub {
			return a.sub < b.sub
		}
		return a.currency < b.currency
	})
	var conds []string
	var args []any
	// The balances are sorted by account, so each sub-account is checked
	// once, before its first balance.
	var checked balanceKey
	for _, w := range all {
		if w.account.SubAccountID != nil && (w.key.user != checked.user || w.key.sub != checked.sub) {
			if err := checkSubAccount(ctx, tx, w.account); err != nil {
				return nil, err
			}
			checked = w.key
		}
		var opened string
		err := tx.QueryRowContext(ctx, `INSERT INTO balances (user_id, sub_account_id, currency_hk) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING RETURNING id`, w.account.UserHK, w.account.SubAccountID, w.currency).Scan(&opened)
		switch {
		case err == nil:
			// A new balance is sealed at once, so that its check below
			// passes (#139).
			if err := rowseal.Reseal(ctx, tx, rowseal.Balances, "id = $1", opened); err != nil {
				return nil, err
			}
		case !errors.Is(err, sql.ErrNoRows):
			return nil, mapError(fmt.Errorf("open the balance of %s in %s: %w", w.account, w.currency, err))
		}
		n := len(args)
		conds = append(conds, fmt.Sprintf("(b.user_id = $%d AND b.sub_account_id IS NOT DISTINCT FROM $%d AND b.currency_hk = $%d)", n+1, n+2, n+3))
		args = append(args, w.account.UserHK, w.account.SubAccountID, w.currency)
	}
	//nolint:gosec // G202: the conditions are built here from placeholders; the values are arguments.
	rows, err := tx.QueryContext(ctx, `SELECT b.id, b.user_id, b.sub_account_id, b.currency_hk, b.available, b.locked, b.pending_deposit,
			b.version, `+rowseal.Balances.Fields("b")+`
		FROM balances b WHERE `+strings.Join(conds, " OR ")+` ORDER BY b.id FOR UPDATE`, args...)
	if err != nil {
		return nil, fmt.Errorf("lock the balances: %w", err)
	}
	out := make(map[balanceKey]*balance.Balance, len(all))
	var sealed []*rowseal.Row
	for rows.Next() {
		b := &balance.Balance{}
		r := rowseal.Balances.NewRow()
		dest := append([]any{&b.ID, &b.UserID, &b.SubAccountID, &b.CurrencyHK, &b.Available, &b.Locked, &b.PendingDeposit, &b.Version},
			r.Dest()...)
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, fmt.Errorf("lock the balances: %w", err)
		}
		b.Total = b.Available.Add(b.Locked)
		out[keyOf(Account{UserHK: b.UserID, SubAccountID: b.SubAccountID}, b.CurrencyHK)] = b
		sealed = append(sealed, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock the balances: %w", err)
	}
	if len(out) != len(all) {
		return nil, fmt.Errorf("lock the balances: found %d of %d", len(out), len(all))
	}
	// A balance changed outside the ledger is not moved in enforce mode
	// (#139): its seal no longer matches.
	for _, r := range sealed {
		if err := rowseal.Check(ctx, rowseal.Balances, r); err != nil {
			return nil, fmt.Errorf("balance %s: %w", r.Key, err)
		}
	}
	return out, nil
}

// mapError turns the errors of the database that a request causes, such as
// an unknown user or currency, into ErrInvalidRequest.
func mapError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
		return fmt.Errorf("%w: %s", ErrInvalidRequest, pgErr.Detail)
	}
	return err
}

// Balance returns the balance of a in a currency, zero when it has none.
func (l Ledger) Balance(ctx context.Context, a Account, currency shared.UUID) (balance.Balance, error) {
	b := balance.Balance{UserID: a.UserHK, SubAccountID: a.SubAccountID, CurrencyHK: currency, Version: 1}
	err := l.DB.QueryRowContext(ctx, `SELECT id, available, locked, pending_deposit, version FROM balances
		WHERE user_id = $1 AND sub_account_id IS NOT DISTINCT FROM $2 AND currency_hk = $3`, a.UserHK, a.SubAccountID, currency).
		Scan(&b.ID, &b.Available, &b.Locked, &b.PendingDeposit, &b.Version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return balance.Balance{}, err
	}
	b.Total = b.Available.Add(b.Locked)
	return b, nil
}

// checkSubAccount refuses the sub-account of an account that does not
// exist, belongs to another user, or is deleted, and keeps it from being
// deleted until the transaction ends. The foreign keys of the balances
// check the owner too (migration 20); this check says which.
func checkSubAccount(ctx context.Context, tx *sql.Tx, a Account) error {
	var deleted bool
	err := tx.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL FROM sub_accounts WHERE id = $1 AND user_id = $2 FOR SHARE`,
		a.SubAccountID, a.UserHK).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: sub-account %s is not one of user %s", ErrInvalidRequest, a.SubAccountID, a.UserHK)
	}
	if err != nil {
		return fmt.Errorf("read the sub-account %s: %w", a.SubAccountID, err)
	}
	if deleted {
		return fmt.Errorf("%w: sub-account %s is deleted", ErrInvalidRequest, a.SubAccountID)
	}
	return nil
}
