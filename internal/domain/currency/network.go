// Package currency holds the domain rules of the currencies that the
// exchange supports. The currencies table is the one registry of them
// (#309, docs/architecture.md#the-asset-registry): balances, markets,
// orders, trades, and custody all refer to a currency by its hk.
package currency

import (
	"errors"
	"math"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// Network is how a currency moves on one network: a row of
// currency_networks. USDT is one currency, held and traded as one balance,
// with a network on TRON and one on Ethereum, each with its own contract,
// decimals, and limits.
type Network struct {
	CurrencyHK shared.UUID `json:"currency_hk" db:"currency_hk"`
	// Network names the chain, such as "bitcoin", "ethereum", or "tron",
	// or "internal" for a fiat currency that moves off chain.
	Network string `json:"network" db:"network"`
	// ContractAddress is the token contract, empty for the native asset of
	// the network and on networks without contracts.
	ContractAddress string `json:"contract_address,omitempty" db:"contract_address"`
	// Decimals is the precision of an amount on the network: 6 for USDT on
	// TRON, 18 for ETH, 0 for IRR.
	Decimals    int                   `json:"decimals" db:"decimals"`
	CustodyMode shared.SettlementMode `json:"custody_mode" db:"custody_mode"`

	DepositEnabled    bool           `json:"deposit_enabled" db:"deposit_enabled"`
	WithdrawalEnabled bool           `json:"withdrawal_enabled" db:"withdrawal_enabled"`
	MinDeposit        shared.Decimal `json:"min_deposit" db:"min_deposit"`
	MinWithdrawal     shared.Decimal `json:"min_withdrawal" db:"min_withdrawal"`
	// MaxWithdrawal of 0 means no maximum.
	MaxWithdrawal shared.Decimal `json:"max_withdrawal" db:"max_withdrawal"`
	WithdrawalFee shared.Decimal `json:"withdrawal_fee" db:"withdrawal_fee"`
	// FeeCurrencyHK is the currency that pays the network fee, such as TRX
	// for a TRC20 token.
	FeeCurrencyHK *shared.UUID `json:"fee_currency_hk,omitempty" db:"fee_currency_hk"`
	// RequiresTag says that a deposit names its account with a tag or memo,
	// as on XRP and XLM.
	RequiresTag bool   `json:"requires_tag" db:"requires_tag"`
	ExplorerURL string `json:"explorer_url,omitempty" db:"explorer_url"`

	CreatedAt shared.Timestamp `json:"created_at" db:"created_at"`
	UpdatedAt shared.Timestamp `json:"updated_at" db:"updated_at"`
}

// RoundQuantity rounds quantity to the decimals of the network. A network
// with 0 decimals, such as that of IRR, rounds to whole units. A negative
// number of decimals is invalid, and one above what the database stores (a
// SMALLINT) cannot be rounded to; the quantity is then unchanged.
func (n *Network) RoundQuantity(qty shared.Decimal) shared.Decimal {
	if n.Decimals < 0 || n.Decimals > math.MaxInt16 {
		return qty
	}
	return shared.Decimal(qty.Decimal().Round(int32(n.Decimals)))
}

// ValidateWithdrawal checks that amount may be withdrawn on the network.
func (n *Network) ValidateWithdrawal(amount shared.Decimal) error {
	if !n.WithdrawalEnabled {
		return ErrWithdrawalDisabled
	}
	if amount.IsNegative() || amount.IsZero() {
		return ErrInvalidWithdrawalAmount
	}
	if amount.LessThan(n.MinWithdrawal) {
		return ErrWithdrawalBelowMinimum
	}
	// A maximum of 0, the column's default, means no maximum.
	if n.MaxWithdrawal.IsPositive() && amount.GreaterThan(n.MaxWithdrawal) {
		return ErrWithdrawalAboveMaximum
	}
	return nil
}

// ValidateDeposit checks that a deposit of amount is credited on the
// network.
func (n *Network) ValidateDeposit(amount shared.Decimal) error {
	if !n.DepositEnabled {
		return ErrDepositDisabled
	}
	if amount.IsNegative() || amount.IsZero() {
		return ErrInvalidDepositAmount
	}
	if amount.LessThan(n.MinDeposit) {
		return ErrDepositBelowMinimum
	}
	return nil
}

// The errors of the checks of a network.
var (
	ErrWithdrawalDisabled      = errors.New("withdrawals are disabled on this network")
	ErrInvalidWithdrawalAmount = errors.New("invalid withdrawal amount")
	ErrWithdrawalBelowMinimum  = errors.New("withdrawal amount below minimum")
	ErrWithdrawalAboveMaximum  = errors.New("withdrawal amount exceeds maximum")
	ErrDepositDisabled         = errors.New("deposits are disabled on this network")
	ErrInvalidDepositAmount    = errors.New("invalid deposit amount")
	ErrDepositBelowMinimum     = errors.New("deposit amount below minimum")
)
