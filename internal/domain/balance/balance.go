// Package balance models the balance of one asset in one account as a state
// machine.
//
// A balance has three components. Available funds can be traded and
// withdrawn. Locked funds are reserved by an open order or a pending
// withdrawal. Pending deposits are detected but not credited yet, so the
// user does not own them yet. The total, which is what the user owns, is
// available plus locked.
//
// Every change goes through Apply with one of the transitions below. A
// transition moves a positive amount between the components, or into or out
// of the balance, and fails without changing anything when a component
// would become negative:
//
//	transition          available  locked  pending  total
//	PENDING_DEPOSIT                          +a
//	DEPOSIT              +a                  -a       +a   (deposit credited)
//	DEPOSIT_FAILED                           -a
//	LOCK                 -a         +a                     (order placed, withdrawal requested)
//	UNLOCK               +a         -a                     (order cancelled, withdrawal failed)
//	TRADE_CREDIT         +a                           +a   (asset received in a trade)
//	TRADE_DEBIT                     -a                -a   (asset given in a trade)
//	FEE                             -a                -a   (fee, locked with the order)
//	WITHDRAWAL                      -a                -a   (withdrawal completed)
//	TRANSFER_OUT                    -a                -a   (sent to another account)
//	TRANSFER_IN          +a                           +a   (received from another account)
//
// Funds therefore only enter the total through a credited deposit, a trade
// credit, or a transfer in, and only leave it from the locked funds. Each
// transition returns a BalanceChangeEvent, from which the ledger (#60)
// records the change.
package balance

import (
	"errors"
	"fmt"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// Balance represents a user's balance for a specific asset
type Balance struct {
	ID           shared.UUID  `json:"id" db:"id"`
	UserID       shared.UUID  `json:"user_id" db:"user_id"`
	SubAccountID *shared.UUID `json:"sub_account_id,omitempty" db:"sub_account_id"`
	CurrencyHK   shared.UUID  `json:"currency_hk" db:"currency_hk"`

	// Balance components
	Available      shared.Decimal `json:"available" db:"available"`             // Free to trade and withdraw
	Locked         shared.Decimal `json:"locked" db:"locked"`                   // Reserved by open orders and pending withdrawals
	PendingDeposit shared.Decimal `json:"pending_deposit" db:"pending_deposit"` // Detected, not credited yet

	// Total is Available + Locked, what the user owns. The database computes
	// it as a generated column.
	Total shared.Decimal `json:"total" db:"total"`

	// Version is raised by every transition. A repository stores a balance
	// with UPDATE ... WHERE version = <the version it read>, so that two
	// concurrent changes cannot both apply.
	Version int `json:"version" db:"version"`

	UpdatedAt shared.Timestamp `json:"updated_at" db:"updated_at"`
}

// delta is the sign of a transition's effect on each component.
type delta struct {
	available, locked, pending int
}

// transitions are the only ways a balance changes.
var transitions = map[BalanceChangeType]delta{
	BalanceChangePendingDep:    {pending: +1},
	BalanceChangeDeposit:       {available: +1, pending: -1},
	BalanceChangeDepositFailed: {pending: -1},
	BalanceChangeLock:          {available: -1, locked: +1},
	BalanceChangeUnlock:        {available: +1, locked: -1},
	BalanceChangeTradeCredit:   {available: +1},
	BalanceChangeTradeDebit:    {locked: -1},
	BalanceChangeFee:           {locked: -1},
	BalanceChangeWithdrawal:    {locked: -1},
	BalanceChangeTransferOut:   {locked: -1},
	BalanceChangeTransferIn:    {available: +1},
}

// TotalEffect is the sign of a transition's effect on the total: +1 for
// funds that enter the balance, -1 for funds that leave it, 0 for funds that
// move within it or are not owned yet. It reports false for an unknown
// transition.
func (t BalanceChangeType) TotalEffect() (int, bool) {
	d, ok := transitions[t]
	return d.available + d.locked, ok
}

// Reference names what caused a change, such as an order, a trade, a
// deposit, or a withdrawal.
type Reference struct {
	ID   *shared.UUID
	Type string // "ORDER", "TRADE", "DEPOSIT", "WITHDRAWAL"
}

// Apply performs one transition of amount, which must be positive, and
// returns the event that describes it. When the transition would make a
// component negative, it returns an error and changes nothing.
func (b *Balance) Apply(change BalanceChangeType, amount shared.Decimal, ref Reference) (BalanceChangeEvent, error) {
	d, ok := transitions[change]
	if !ok {
		return BalanceChangeEvent{}, fmt.Errorf("%w: %q", ErrUnknownTransition, change)
	}
	if !amount.IsPositive() {
		return BalanceChangeEvent{}, ErrInvalidAmount
	}

	available, err := move(b.Available, d.available, amount, ErrInsufficientBalance)
	if err != nil {
		return BalanceChangeEvent{}, err
	}
	locked, err := move(b.Locked, d.locked, amount, ErrInsufficientLocked)
	if err != nil {
		return BalanceChangeEvent{}, err
	}
	pending, err := move(b.PendingDeposit, d.pending, amount, ErrInsufficientPending)
	if err != nil {
		return BalanceChangeEvent{}, err
	}

	event := BalanceChangeEvent{
		BalanceID:     b.ID,
		UserID:        b.UserID,
		SubAccountID:  b.SubAccountID,
		CurrencyHK:    b.CurrencyHK,
		ChangeType:    change,
		Amount:        amount,
		OldAvailable:  b.Available,
		NewAvailable:  available,
		OldLocked:     b.Locked,
		NewLocked:     locked,
		OldPending:    b.PendingDeposit,
		NewPending:    pending,
		OldVersion:    b.Version,
		NewVersion:    b.Version + 1,
		ReferenceID:   ref.ID,
		ReferenceType: ref.Type,
		Timestamp:     shared.Now(),
	}
	b.Available, b.Locked, b.PendingDeposit = available, locked, pending
	b.Total = b.Available.Add(b.Locked)
	b.Version++
	return event, nil
}

// move adds or subtracts amount from value as sign says, and refuses a
// negative result with insufficient.
func move(value shared.Decimal, sign int, amount shared.Decimal, insufficient error) (shared.Decimal, error) {
	switch sign {
	case +1:
		return value.Add(amount), nil
	case -1:
		if value.LessThan(amount) {
			return value, insufficient
		}
		return value.Sub(amount), nil
	}
	return value, nil
}

// CanWithdraw reports whether a positive amount can be locked for a
// withdrawal.
func (b *Balance) CanWithdraw(amount shared.Decimal) bool {
	return amount.IsPositive() && b.Available.GreaterThanOrEqual(amount)
}

// CanTrade reports whether a positive amount can be locked for an order.
func (b *Balance) CanTrade(amount shared.Decimal) bool {
	return amount.IsPositive() && b.Available.GreaterThanOrEqual(amount)
}

// BalanceSnapshot for event sourcing
type BalanceSnapshot struct {
	BalanceID      shared.UUID      `json:"balance_id"`
	UserID         shared.UUID      `json:"user_id"`
	SubAccountID   *shared.UUID     `json:"sub_account_id,omitempty"`
	CurrencyHK     shared.UUID      `json:"currency_hk"`
	Available      shared.Decimal   `json:"available"`
	Locked         shared.Decimal   `json:"locked"`
	PendingDeposit shared.Decimal   `json:"pending_deposit"`
	Total          shared.Decimal   `json:"total"`
	Version        int              `json:"version"`
	Timestamp      shared.Timestamp `json:"timestamp"`
}

// BalanceChangeEvent describes one transition, with the components before
// and after it.
type BalanceChangeEvent struct {
	EventID      shared.UUID       `json:"event_id"`
	BalanceID    shared.UUID       `json:"balance_id"`
	UserID       shared.UUID       `json:"user_id"`
	SubAccountID *shared.UUID      `json:"sub_account_id,omitempty"`
	CurrencyHK   shared.UUID       `json:"currency_hk"`
	ChangeType   BalanceChangeType `json:"change_type"`
	Amount       shared.Decimal    `json:"amount"`

	// Before/after
	OldAvailable shared.Decimal `json:"old_available"`
	NewAvailable shared.Decimal `json:"new_available"`
	OldLocked    shared.Decimal `json:"old_locked"`
	NewLocked    shared.Decimal `json:"new_locked"`
	OldPending   shared.Decimal `json:"old_pending_deposit"`
	NewPending   shared.Decimal `json:"new_pending_deposit"`

	// The versions before and after, for the optimistic lock of the store.
	OldVersion int `json:"old_version"`
	NewVersion int `json:"new_version"`

	// Reference
	ReferenceID   *shared.UUID `json:"reference_id,omitempty"`   // OrderID, TradeID, WithdrawalID
	ReferenceType string       `json:"reference_type,omitempty"` // "ORDER", "TRADE", "WITHDRAWAL", "DEPOSIT"

	Timestamp shared.Timestamp `json:"timestamp"`
}

type BalanceChangeType string

const (
	BalanceChangePendingDep    BalanceChangeType = "PENDING_DEPOSIT" // Deposit detected
	BalanceChangeDeposit       BalanceChangeType = "DEPOSIT"         // Deposit credited
	BalanceChangeDepositFailed BalanceChangeType = "DEPOSIT_FAILED"  // Detected deposit that will not be credited
	BalanceChangeLock          BalanceChangeType = "LOCK"            // Order placed or withdrawal requested
	BalanceChangeUnlock        BalanceChangeType = "UNLOCK"          // Order cancelled or withdrawal failed
	BalanceChangeTradeCredit   BalanceChangeType = "TRADE_CREDIT"    // Asset received in a trade
	BalanceChangeTradeDebit    BalanceChangeType = "TRADE_DEBIT"     // Asset given in a trade, from the lock
	BalanceChangeFee           BalanceChangeType = "FEE"             // Fee charged, from the lock
	BalanceChangeWithdrawal    BalanceChangeType = "WITHDRAWAL"      // Withdrawal completed, from the lock
	BalanceChangeTransferOut   BalanceChangeType = "TRANSFER_OUT"    // Sent to another account, from the lock
	BalanceChangeTransferIn    BalanceChangeType = "TRANSFER_IN"     // Received from another account
)

func (b BalanceChangeType) String() string {
	return string(b)
}

var (
	ErrInsufficientBalance = errors.New("insufficient available balance")
	ErrInsufficientLocked  = errors.New("insufficient locked balance")
	ErrInsufficientPending = errors.New("insufficient pending deposit")
	ErrInvalidAmount       = errors.New("amount must be positive")
	ErrUnknownTransition   = errors.New("unknown balance transition")
)
