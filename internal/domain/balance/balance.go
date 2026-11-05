package balance

import (
	"errors"

	"encrypted-db/internal/domain/asset"
	"encrypted-db/internal/domain/shared"
)

// Balance represents a user's balance for a specific asset
type Balance struct {
	ID           shared.UUID  `json:"id" db:"id"`
	UserID       shared.UUID  `json:"user_id" db:"user_id"`
	SubAccountID *shared.UUID `json:"sub_account_id,omitempty" db:"sub_account_id"`
	AssetID      shared.UUID  `json:"asset_id" db:"asset_id"`

	// Balance components
	Available      shared.Decimal `json:"available" db:"available"`             // Free to trade/withdraw
	Locked         shared.Decimal `json:"locked" db:"locked"`                   // Locked in orders/withdrawals
	OnChain        shared.Decimal `json:"on_chain" db:"on_chain"`               // Confirmed on-chain (for deposits)
	PendingDeposit shared.Decimal `json:"pending_deposit" db:"pending_deposit"` // Unconfirmed deposits

	// Totals
	Total shared.Decimal `json:"total" db:"total"` // Available + Locked + OnChain + PendingDeposit

	// Version for optimistic locking
	Version int `json:"version" db:"version"`

	// Relations
	Asset *asset.Asset `json:"asset,omitempty" db:"-"`

	UpdatedAt shared.Timestamp `json:"updated_at" db:"updated_at"`
}

// LockAmount locks amount for trading/withdrawal
// Returns error if insufficient available balance
func (b *Balance) LockAmount(amount shared.Decimal) error {
	if amount.IsNegative() {
		return ErrNegativeAmount
	}
	if b.Available.LessThan(amount) {
		return ErrInsufficientBalance
	}
	b.Available = b.Available.Sub(amount)
	b.Locked = b.Locked.Add(amount)
	b.recalcTotal()
	b.Version++
	return nil
}

// UnlockAmount releases locked amount back to available
func (b *Balance) UnlockAmount(amount shared.Decimal) error {
	if amount.IsNegative() {
		return ErrNegativeAmount
	}
	if b.Locked.LessThan(amount) {
		return ErrInsufficientLocked
	}
	b.Locked = b.Locked.Sub(amount)
	b.Available = b.Available.Add(amount)
	b.recalcTotal()
	b.Version++
	return nil
}

// AddAvailable increases available balance (e.g., from trade fill, deposit)
func (b *Balance) AddAvailable(amount shared.Decimal) error {
	if amount.IsNegative() {
		return ErrNegativeAmount
	}
	b.Available = b.Available.Add(amount)
	b.recalcTotal()
	b.Version++
	return nil
}

// AddLocked increases locked balance (e.g., new order placed)
func (b *Balance) AddLocked(amount shared.Decimal) error {
	if amount.IsNegative() {
		return ErrNegativeAmount
	}
	b.Locked = b.Locked.Add(amount)
	b.recalcTotal()
	b.Version++
	return nil
}

// AddOnChain increases on-chain balance (confirmed deposit)
func (b *Balance) AddOnChain(amount shared.Decimal) error {
	if amount.IsNegative() {
		return ErrNegativeAmount
	}
	b.OnChain = b.OnChain.Add(amount)
	b.recalcTotal()
	b.Version++
	return nil
}

// AddPendingDeposit increases pending deposit (unconfirmed)
func (b *Balance) AddPendingDeposit(amount shared.Decimal) error {
	if amount.IsNegative() {
		return ErrNegativeAmount
	}
	b.PendingDeposit = b.PendingDeposit.Add(amount)
	b.recalcTotal()
	b.Version++
	return nil
}

// ConfirmDeposit moves amount from pending to on-chain
func (b *Balance) ConfirmDeposit(amount shared.Decimal) error {
	if amount.IsNegative() {
		return ErrNegativeAmount
	}
	if b.PendingDeposit.LessThan(amount) {
		return ErrInsufficientPending
	}
	b.PendingDeposit = b.PendingDeposit.Sub(amount)
	b.OnChain = b.OnChain.Add(amount)
	b.recalcTotal()
	b.Version++
	return nil
}

// SettleWithdrawal reduces on-chain balance for withdrawal
func (b *Balance) SettleWithdrawal(amount shared.Decimal) error {
	if amount.IsNegative() {
		return ErrNegativeAmount
	}
	if b.OnChain.LessThan(amount) {
		return ErrInsufficientOnChain
	}
	b.OnChain = b.OnChain.Sub(amount)
	b.recalcTotal()
	b.Version++
	return nil
}

// recalcTotal recalculates total balance
func (b *Balance) recalcTotal() {
	b.Total = b.Available.Add(b.Locked).Add(b.OnChain).Add(b.PendingDeposit)
}

// CanWithdraw checks if amount can be withdrawn
func (b *Balance) CanWithdraw(amount shared.Decimal) bool {
	return b.Available.GreaterThanOrEqual(amount)
}

// CanTrade checks if amount is available for trading
func (b *Balance) CanTrade(amount shared.Decimal) bool {
	return b.Available.GreaterThanOrEqual(amount)
}

// BalanceSnapshot for event sourcing
type BalanceSnapshot struct {
	BalanceID      shared.UUID      `json:"balance_id"`
	UserID         shared.UUID      `json:"user_id"`
	SubAccountID   *shared.UUID     `json:"sub_account_id,omitempty"`
	AssetID        shared.UUID      `json:"asset_id"`
	Available      shared.Decimal   `json:"available"`
	Locked         shared.Decimal   `json:"locked"`
	OnChain        shared.Decimal   `json:"on_chain"`
	PendingDeposit shared.Decimal   `json:"pending_deposit"`
	Total          shared.Decimal   `json:"total"`
	Version        int              `json:"version"`
	Timestamp      shared.Timestamp `json:"timestamp"`
}

// BalanceChangeEvent for event sourcing
type BalanceChangeEvent struct {
	EventID      shared.UUID       `json:"event_id"`
	BalanceID    shared.UUID       `json:"balance_id"`
	UserID       shared.UUID       `json:"user_id"`
	SubAccountID *shared.UUID      `json:"sub_account_id,omitempty"`
	AssetID      shared.UUID       `json:"asset_id"`
	ChangeType   BalanceChangeType `json:"change_type"`
	Amount       shared.Decimal    `json:"amount"`

	// Before/after
	OldAvailable shared.Decimal `json:"old_available"`
	NewAvailable shared.Decimal `json:"new_available"`
	OldLocked    shared.Decimal `json:"old_locked"`
	NewLocked    shared.Decimal `json:"new_locked"`
	OldOnChain   shared.Decimal `json:"old_on_chain"`
	NewOnChain   shared.Decimal `json:"new_on_chain"`
	OldPending   shared.Decimal `json:"old_pending_deposit"`
	NewPending   shared.Decimal `json:"new_pending_deposit"`

	// Reference
	ReferenceID   *shared.UUID `json:"reference_id,omitempty"`   // OrderID, TradeID, WithdrawalID
	ReferenceType string       `json:"reference_type,omitempty"` // "ORDER", "TRADE", "WITHDRAWAL", "DEPOSIT"

	Timestamp shared.Timestamp `json:"timestamp"`
}

type BalanceChangeType string

const (
	BalanceChangeLock        BalanceChangeType = "LOCK"            // Order placed
	BalanceChangeUnlock      BalanceChangeType = "UNLOCK"          // Order cancelled
	BalanceChangeTradeCredit BalanceChangeType = "TRADE_CREDIT"    // Trade fill credit
	BalanceChangeTradeDebit  BalanceChangeType = "TRADE_DEBIT"     // Trade fill debit
	BalanceChangeFee         BalanceChangeType = "FEE"             // Fee charged
	BalanceChangeDeposit     BalanceChangeType = "DEPOSIT"         // Deposit confirmed
	BalanceChangePendingDep  BalanceChangeType = "PENDING_DEPOSIT" // Deposit detected
	BalanceChangeWithdrawal  BalanceChangeType = "WITHDRAWAL"      // Withdrawal processed
	BalanceChangeSettlement  BalanceChangeType = "SETTLEMENT"      // On-chain settlement
)

func (b BalanceChangeType) String() string {
	return string(b)
}

var (
	ErrInsufficientBalance = errors.New("insufficient available balance")
	ErrInsufficientLocked  = errors.New("insufficient locked balance")
	ErrInsufficientOnChain = errors.New("insufficient on-chain balance")
	ErrInsufficientPending = errors.New("insufficient pending deposit")
	ErrNegativeAmount      = errors.New("amount cannot be negative")
)
