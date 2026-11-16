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
//
// Locked funds are held per reservation (#407): a Lock holds funds of a
// balance for its reference, such as a withdrawal or an order, and a move
// that takes locked funds (a release, a withdrawal, a trade's debit and
// fee, a transfer, a conversion) must name the reference of the hold it
// ends, and may not take more than that hold has left. The holds are not a
// table: each is the sum of the locks of its reference in the balance less
// what moves of that reference took, read from the entries under the lock
// of the balance, and from the moves before it in the same entry. So a
// caller with a wrong reference cannot free or spend the funds that
// another operation reserved.
package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
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
	KindConversion      = "conversion"
	// The moves of the exchange's own funds to and from the venues (#385).
	KindVenueDeposit    = "venue_deposit"
	KindVenueWithdrawal = "venue_withdrawal"
	KindVenueFee        = "venue_fee"
	// A fill of a routed order at a venue (#387).
	KindVenueFill = "venue_fill"
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
// cancelled or a withdrawal fails. ref is the reference that locked them:
// a release takes from that hold only, and no more than it has left
// (#407).
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
// OTC: the user gives Give, from funds locked with the order, and receives
// Receive, available. Fee is in the currency of Give or of Receive, never
// of another (#407). In Give, it is taken from the locked funds of the
// order, as Give is. In Receive, it is netted: the user is credited
// Receive less Fee, which must be positive, and takes nothing from any
// hold (#62). A Fee of zero is none.
type Trade struct {
	Give, Receive, Fee Amount
}

// SettleTrade settles a trade of a against the exchange's inventory: what
// the user gives enters the inventory, what the user receives leaves it,
// and the fee goes to the fees account. ref is the order whose hold the
// trade ends, as it was locked (#407): what the trade gives and a fee in
// its currency are taken from that hold, and no more than it has left. The
// trade itself is named by key.
func (l Ledger) SettleTrade(ctx context.Context, key string, a Account, t Trade, ref Ref) (Result, error) {
	e, err := trade(key, a, t, ref)
	if err != nil {
		return Result{}, err
	}
	return l.record(ctx, e)
}

// trade is the entry of a settled trade.
func trade(key string, a Account, t Trade, ref Ref) (entry, error) {
	if t.Give.Currency == t.Receive.Currency {
		return entry{}, fmt.Errorf("%w: a trade gives and receives %s", ErrInvalidRequest, t.Give.Currency)
	}
	netted := !t.Fee.Value.IsZero() && t.Fee.Currency == t.Receive.Currency
	if !t.Fee.Value.IsZero() && !netted && t.Fee.Currency != t.Give.Currency {
		return entry{}, fmt.Errorf("%w: the fee of a trade is in %s or %s, not in %s",
			ErrInvalidRequest, t.Give.Currency, t.Receive.Currency, t.Fee.Currency)
	}
	if t.Fee.Value.IsNegative() {
		return entry{}, fmt.Errorf("%w: a negative fee", ErrInvalidRequest)
	}
	credit := t.Receive.Value
	if netted {
		credit = credit.Sub(t.Fee.Value)
	}
	e := entry{kind: KindTrade, key: key, ref: ref,
		moves: []move{
			{a, t.Give.Currency, balance.BalanceChangeTradeDebit, t.Give.Value},
			{a, t.Receive.Currency, balance.BalanceChangeTradeCredit, credit},
		},
		postings: []posting{
			{AccountInventory, t.Give.Currency, t.Give.Value},
			{AccountInventory, t.Receive.Currency, t.Receive.Value.Neg()},
		},
	}
	switch {
	case netted:
		e.postings = append(e.postings, posting{AccountFees, t.Fee.Currency, t.Fee.Value})
	case !t.Fee.Value.IsZero():
		e.moves = append(e.moves, move{a, t.Fee.Currency, balance.BalanceChangeFee, t.Fee.Value})
		e.postings = append(e.postings, posting{AccountFees, t.Fee.Currency, t.Fee.Value})
	}
	return e, nil
}

// VenueAccount is the house account of the exchange at the venue name
// (#385), one per currency: the negative of what we hold there, as the
// custody account is of what custody holds.
func VenueAccount(name string) string { return venueAccountPrefix + name }

const venueAccountPrefix = "venue:"

// IsVenueAccount reports an account of VenueAccount.
func IsVenueAccount(account string) bool { return strings.HasPrefix(account, venueAccountPrefix) }

// VenueDeposit records a deposit of the exchange's own funds from custody to
// the venue, once the venue credited it: custody holds amount less, and
// the venue amount more. ref is the move, such as a deposit to the venue.
func (l Ledger) VenueDeposit(ctx context.Context, key, venue string, amount Amount, ref Ref) (Result, error) {
	if !amount.Value.IsPositive() {
		return Result{}, fmt.Errorf("%w: a move of a venue of %s", ErrInvalidRequest, amount.Value)
	}
	return l.record(ctx, entry{kind: KindVenueDeposit, key: key, ref: ref, postings: []posting{
		{AccountCustody, amount.Currency, amount.Value},
		{VenueAccount(venue), amount.Currency, amount.Value.Neg()},
	}})
}

// VenueWithdrawal records a withdrawal of the exchange's funds from the
// venue to custody, once custody received it.
func (l Ledger) VenueWithdrawal(ctx context.Context, key, venue string, amount Amount, ref Ref) (Result, error) {
	if !amount.Value.IsPositive() {
		return Result{}, fmt.Errorf("%w: a move of a venue of %s", ErrInvalidRequest, amount.Value)
	}
	return l.record(ctx, entry{kind: KindVenueWithdrawal, key: key, ref: ref, postings: []posting{
		{VenueAccount(venue), amount.Currency, amount.Value},
		{AccountCustody, amount.Currency, amount.Value.Neg()},
	}})
}

// VenueFee records a fee that the venue charged us, as it reported it: the
// venue holds fee less, which the inventory, the exchange's own funds,
// pays.
func (l Ledger) VenueFee(ctx context.Context, key, venue string, fee Amount, ref Ref) (Result, error) {
	if !fee.Value.IsPositive() {
		return Result{}, fmt.Errorf("%w: a move of a venue of %s", ErrInvalidRequest, fee.Value)
	}
	return l.record(ctx, entry{kind: KindVenueFee, key: key, ref: ref, postings: []posting{
		{VenueAccount(venue), fee.Currency, fee.Value},
		{AccountInventory, fee.Currency, fee.Value.Neg()},
	}})
}

// VenueFill records a fill of a routed order at the venue (#387), which
// covers an OTC trade that the inventory settled with the user: the venue
// holds got more and gave less, and the inventory the reverse, so that the
// inventory is whole again in what it gave the user. A buy at the venue
// gets the base currency and gives the quote; a sell the reverse.
func (l Ledger) VenueFill(ctx context.Context, key, venue string, got, gave Amount, ref Ref) (Result, error) {
	e, err := venueFill(key, venue, got, gave, ref)
	if err != nil {
		return Result{}, err
	}
	return l.record(ctx, e)
}

func venueFill(key, venue string, got, gave Amount, ref Ref) (entry, error) {
	if !got.Value.IsPositive() || !gave.Value.IsPositive() {
		return entry{}, fmt.Errorf("%w: a fill of a venue of %s for %s", ErrInvalidRequest, got.Value, gave.Value)
	}
	if got.Currency == gave.Currency {
		return entry{}, fmt.Errorf("%w: a fill of a venue gets and gives one currency", ErrInvalidRequest)
	}
	return entry{kind: KindVenueFill, key: key, ref: ref, postings: []posting{
		{VenueAccount(venue), got.Currency, got.Value.Neg()},
		{AccountInventory, got.Currency, got.Value},
		{VenueAccount(venue), gave.Currency, gave.Value},
		{AccountInventory, gave.Currency, gave.Value.Neg()},
	}}, nil
}

// VenueHoldings returns what the books say the exchange holds at the venue
// name in each currency (#385), by the currency's hk: the negative of the
// venue's account. A currency that was never moved there is absent.
func (l Ledger) VenueHoldings(ctx context.Context, name string) (map[string]shared.Decimal, error) {
	rows, err := l.DB.QueryContext(ctx, `SELECT currency_hk::text, (-SUM(amount::numeric))::text
		FROM ledger_postings WHERE account = $1 GROUP BY currency_hk`, VenueAccount(name))
	if err != nil {
		return nil, fmt.Errorf("ledger: read the holdings of venue %s: %w", name, err)
	}
	defer rows.Close()
	out := map[string]shared.Decimal{}
	for rows.Next() {
		var hk, sum string
		if err := rows.Scan(&hk, &sum); err != nil {
			return nil, fmt.Errorf("ledger: read the holdings of venue %s: %w", name, err)
		}
		d, err := shared.NewDecimalFromString(sum)
		if err != nil {
			return nil, fmt.Errorf("ledger: read the holdings of venue %s: %w", name, err)
		}
		out[hk] = d
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: read the holdings of venue %s: %w", name, err)
	}
	return out, nil
}

// Conversion converts the available funds of a in one currency into
// another, as a currency migration does (#215): From leaves the balance,
// through a lock as every debit does, and To enters it. Custody must then
// hold From less of its currency and To + Dust more of the other, which the
// exchange receives from the migration of the project; Dust, the part of
// the converted amount under the precision of the target, goes to the fees
// account. To or Dust of zero is none, not both.
type Conversion struct {
	From, To Amount
	Dust     shared.Decimal
}

// Convert records a conversion of a.
func (l Ledger) Convert(ctx context.Context, key string, a Account, c Conversion, ref Ref) (Result, error) {
	e, err := conversion(key, a, c, ref)
	if err != nil {
		return Result{}, err
	}
	return l.record(ctx, e)
}

// conversion is the entry of a conversion.
func conversion(key string, a Account, c Conversion, ref Ref) (entry, error) {
	if c.From.Currency == c.To.Currency {
		return entry{}, fmt.Errorf("%w: a conversion of %s into itself", ErrInvalidRequest, c.From.Currency)
	}
	if c.To.Value.IsNegative() || c.Dust.IsNegative() || c.To.Value.IsZero() && c.Dust.IsZero() {
		return entry{}, fmt.Errorf("%w: a conversion credits a positive amount or dust", ErrInvalidRequest)
	}
	e := entry{kind: KindConversion, key: key, ref: ref,
		moves: []move{
			{a, c.From.Currency, balance.BalanceChangeLock, c.From.Value},
			{a, c.From.Currency, balance.BalanceChangeConversionOut, c.From.Value},
		},
		postings: []posting{
			{AccountCustody, c.From.Currency, c.From.Value},
			{AccountCustody, c.To.Currency, c.To.Value.Add(c.Dust).Neg()},
		},
	}
	if c.To.Value.IsPositive() {
		e.moves = append(e.moves, move{a, c.To.Currency, balance.BalanceChangeConversionIn, c.To.Value})
	}
	if c.Dust.IsPositive() {
		e.postings = append(e.postings, posting{AccountFees, c.To.Currency, c.Dust})
	}
	return e, nil
}

// Withdraw completes a withdrawal of a, whose amount and fee were locked
// when it was requested: the amount leaves custody, and the fee goes to the
// fees account. A fee of zero is none. ref is the withdrawal's, whose hold
// the amount and the fee are taken from, and no more than it has left
// (#407).
func (l Ledger) Withdraw(ctx context.Context, key string, a Account, amount Amount, fee shared.Decimal, ref Ref) (Result, error) {
	return l.record(ctx, withdrawal(key, a, amount, fee, ref))
}

// withdrawal is the entry of a completed withdrawal.
func withdrawal(key string, a Account, amount Amount, fee shared.Decimal, ref Ref) entry {
	e := entry{kind: KindWithdrawal, key: key, ref: ref,
		moves:    []move{{a, amount.Currency, balance.BalanceChangeWithdrawal, amount.Value}},
		postings: []posting{{AccountCustody, amount.Currency, amount.Value}},
	}
	if !fee.IsZero() {
		e.moves = append(e.moves, move{a, amount.Currency, balance.BalanceChangeFee, fee})
		e.postings = append(e.postings, posting{AccountFees, amount.Currency, fee})
	}
	return e
}

// venueName is the name of a venue, as the configuration names it.
var venueName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}$`)

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
		if IsVenueAccount(p.account) && !venueName.MatchString(strings.TrimPrefix(p.account, venueAccountPrefix)) {
			return fmt.Errorf("%w: %q is not the name of a venue", ErrInvalidRequest, p.account)
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

	if err := checkPrecision(ctx, tx, e.moves); err != nil {
		return Result{}, err
	}
	balances, err := lockBalances(ctx, tx, e.moves)
	if err != nil {
		return Result{}, err
	}
	if err := checkHolds(ctx, tx, e, balances); err != nil {
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

// checkPrecision refuses a move of a balance finer than the precision of
// its currency (#422): the most decimal places of its networks, as
// Precision reads it, so that no balance holds dust that no network can
// withdraw. A currency without networks keeps MaxConversionPlaces. The
// postings to the exchange's accounts may be finer, as the dust of a
// conversion is.
func checkPrecision(ctx context.Context, tx *sql.Tx, moves []move) error {
	seen := map[shared.UUID]bool{}
	var currencies []string
	for _, m := range moves {
		if !seen[m.currency] {
			seen[m.currency] = true
			currencies = append(currencies, m.currency.String())
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT c::text, MAX(n.decimals) FROM unnest($1::uuid[]) AS c
		LEFT JOIN currency_networks n ON n.currency_hk = c GROUP BY c`, currencies)
	if err != nil {
		return mapError(fmt.Errorf("read the precision of the currencies: %w", err))
	}
	defer rows.Close()
	places := map[string]int32{}
	for rows.Next() {
		var hk string
		var p sql.NullInt32
		if err := rows.Scan(&hk, &p); err != nil {
			return fmt.Errorf("read the precision of the currencies: %w", err)
		}
		places[hk] = MaxConversionPlaces
		if p.Valid {
			places[hk] = p.Int32
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the precision of the currencies: %w", err)
	}
	for _, m := range moves {
		p, ok := places[m.currency.String()]
		if !ok {
			p = MaxConversionPlaces
		}
		if d := m.amount.Decimal(); !d.Equal(d.Truncate(p)) {
			return fmt.Errorf("%w: %s of %s in %s is finer than its %d decimal places", ErrInvalidRequest, m.change, m.amount, m.currency, p)
		}
	}
	return nil
}

// takesLocked reports whether a change takes locked funds, which must come
// from the hold of the entry's reference (#407).
func takesLocked(c balance.BalanceChangeType) bool {
	switch c {
	case balance.BalanceChangeUnlock, balance.BalanceChangeTradeDebit, balance.BalanceChangeFee,
		balance.BalanceChangeWithdrawal, balance.BalanceChangeTransferOut, balance.BalanceChangeConversionOut:
		return true
	}
	return false
}

// checkHolds checks that every move of e that takes locked funds takes
// them from the hold of e's reference in its balance, and no more than the
// hold has left (#407). It runs under the locks of the balances, so the
// holds cannot change meanwhile. A move takes first from what the entry
// itself locked before it, as a transfer and a conversion do, and only
// then reads the hold that earlier entries left, once per balance.
func checkHolds(ctx context.Context, tx *sql.Tx, e entry, balances map[balanceKey]*balance.Balance) error {
	held := map[balanceKey]shared.Decimal{}
	read := map[balanceKey]bool{}
	for _, m := range e.moves {
		k := keyOf(m.account, m.currency)
		left, ok := held[k]
		if !ok {
			left = shared.NewDecimalFromInt64(0)
		}
		switch {
		case m.change == balance.BalanceChangeLock:
			held[k] = left.Add(m.amount)
			continue
		case !takesLocked(m.change):
			continue
		}
		// Without a reference, only what the entry locked itself is held
		// for it.
		if left.LessThan(m.amount) && !read[k] && e.ref.Type != "" {
			stored, err := storedHold(ctx, tx, balances[k], e.ref)
			if err != nil {
				return err
			}
			left, read[k] = left.Add(stored), true
		}
		if left.LessThan(m.amount) {
			return fmt.Errorf("%w: %s of %s in %s for %s %s, whose hold has %s left",
				ErrInsufficientLocked, m.change, m.amount, m.currency, e.ref.Type, e.ref.ID, left)
		}
		held[k] = left.Sub(m.amount)
	}
	return nil
}

// storedHold returns what the hold of ref in the balance b has left after
// the entries before: its locks less what its moves took. The entries of a
// reference come from the index ledger_entries_reference, and their changes
// from balance_changes_entry.
//
// The history has no seal yet (#366), but the balance has, over its
// version (#139), and every change of the ledger made a version of its
// balance, at most the current one: a version is unique per balance, so a
// change written in SQL to make up a hold must come after it. A balance
// with such a change is refused, and the reconciliation reports it, since
// its changes no longer replay to it.
func storedHold(ctx context.Context, tx *sql.Tx, b *balance.Balance, ref Ref) (shared.Decimal, error) {
	var left shared.Decimal
	var ahead bool
	err := tx.QueryRowContext(ctx, `SELECT
			(SELECT COALESCE(SUM(CASE WHEN c.change_type = 'LOCK' THEN c.amount ELSE -c.amount END), 0)
			FROM ledger_entries e JOIN balance_changes c ON c.entry_id = e.id
			WHERE e.reference_type = $2 AND e.reference_id = $3 AND c.balance_id = $1
			  AND c.change_type IN ('LOCK', 'UNLOCK', 'TRADE_DEBIT', 'FEE', 'WITHDRAWAL', 'TRANSFER_OUT', 'CONVERSION_DEBIT')),
			EXISTS (SELECT 1 FROM balance_changes c WHERE c.balance_id = $1 AND c.version > $4)`,
		b.ID, ref.Type, ref.ID, b.Version).Scan(&left, &ahead)
	if err != nil {
		return shared.Decimal{}, fmt.Errorf("read the hold of %s %s: %w", ref.Type, ref.ID, err)
	}
	if ahead {
		log.Printf("SECURITY: balance %s has changes after its sealed version %d, which no entry of the ledger made; "+
			"its holds are not used", b.ID, b.Version)
		return shared.Decimal{}, fmt.Errorf("%w: balance %s has changes after its version %d", ErrUnverified, b.ID, b.Version)
	}
	return left, nil
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
	// An entry of the exchange's own accounts only, such as a move to a
	// venue (#385), touches no balance.
	if len(all) == 0 {
		return map[balanceKey]*balance.Balance{}, nil
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
	// FOR NO KEY UPDATE, not FOR UPDATE: an entry changes no key of a
	// balance, and FOR UPDATE would also wait for the FOR KEY SHARE locks
	// that foreign key checks take, such as those of the snapshots of the
	// hourly audit. Those lock balances in another order than this one, so
	// an audit and an operation could deadlock.
	//nolint:gosec // G202: the conditions are built here from placeholders; the values are arguments.
	rows, err := tx.QueryContext(ctx, `SELECT b.id, b.user_id, b.sub_account_id, b.currency_hk, b.available, b.locked, b.pending_deposit,
			b.version, `+rowseal.Balances.Fields("b")+`
		FROM balances b WHERE `+strings.Join(conds, " OR ")+` ORDER BY b.id FOR NO KEY UPDATE`, args...)
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
	if !errors.As(err, &pgErr) {
		return err
	}
	switch {
	case pgErr.Code == "23503": // foreign_key_violation
		return fmt.Errorf("%w: %s", ErrInvalidRequest, pgErr.Detail)
	case pgErr.Code == "22003", // numeric_value_out_of_range
		pgErr.Code == "23514" && pgErr.ConstraintName == "exact_decimal_check": // an amount beyond 36 decimals (#422)
		return fmt.Errorf("%w: an amount out of range: %s", ErrInvalidRequest, pgErr.Message)
	}
	return err
}

// Balance returns the balance of a in a currency, zero when it has none.
func (l Ledger) Balance(ctx context.Context, a Account, currency shared.UUID) (balance.Balance, error) {
	return balanceIn(ctx, l.DB, a, currency)
}

// RowQuerier reads one row: the database, a transaction, or one connection
// of the pool.
type RowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// BalanceIn is Balance in q, such as one connection of the pool that a
// request holds for its reads (#455).
func BalanceIn(ctx context.Context, q RowQuerier, a Account, currency shared.UUID) (balance.Balance, error) {
	return balanceIn(ctx, q, a, currency)
}

// balanceIn is Balance in q, the database or a transaction.
func balanceIn(ctx context.Context, q RowQuerier, a Account, currency shared.UUID) (balance.Balance, error) {
	b := balance.Balance{UserID: a.UserHK, SubAccountID: a.SubAccountID, CurrencyHK: currency, Version: 1}
	err := q.QueryRowContext(ctx, `SELECT id, available, locked, pending_deposit, version FROM balances
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
