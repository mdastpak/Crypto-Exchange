package currencies

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
)

// The lifecycle of a currency (#216). A currency is active until a
// delisting takes it, step by step, to delisted. Each state says what users
// may still do with the currency, as capabilities, and an administrator can
// override each capability. Every flow that moves a currency checks its
// capability with Check, so the capabilities are the single source of truth.

// Lifecycle is the lifecycle state of a currency.
type Lifecycle string

// The states, in the order a delisting goes through them.
const (
	LifecycleActive           Lifecycle = "active"
	LifecycleDelistAnnounced  Lifecycle = "delist_announced"
	LifecycleDepositsDisabled Lifecycle = "deposits_disabled"
	LifecycleSellOnly         Lifecycle = "sell_only"
	LifecycleTradingHalted    Lifecycle = "trading_halted"
	LifecycleWithdrawalOnly   Lifecycle = "withdrawal_only"
	LifecycleFrozen           Lifecycle = "frozen"
	LifecycleDelisted         Lifecycle = "delisted"
)

// Lifecycles are the states in their order.
var Lifecycles = []Lifecycle{
	LifecycleActive, LifecycleDelistAnnounced, LifecycleDepositsDisabled, LifecycleSellOnly,
	LifecycleTradingHalted, LifecycleWithdrawalOnly, LifecycleFrozen, LifecycleDelisted,
}

// Rank is the position of l in Lifecycles, or -1 for an unknown state.
func (l Lifecycle) Rank() int { return slices.Index(Lifecycles, l) }

// Reversible reports whether a delisting at l can still be cancelled: at
// frozen, balances start to be settled, which cannot be undone.
func (l Lifecycle) Reversible() bool {
	return l.Rank() >= 0 && l.Rank() < LifecycleFrozen.Rank()
}

// Capability is something users may do with a currency.
type Capability string

const (
	CapabilityDeposit     Capability = "deposit"
	CapabilityWithdraw    Capability = "withdraw"
	CapabilityBuy         Capability = "buy"
	CapabilitySell        Capability = "sell"
	CapabilityConvertInto Capability = "convert_into"
	CapabilityConvertFrom Capability = "convert_from"
	// CapabilityListed is being in the public lists.
	CapabilityListed Capability = "listed"
)

// Capabilities are every capability.
var Capabilities = []Capability{
	CapabilityDeposit, CapabilityWithdraw, CapabilityBuy, CapabilitySell,
	CapabilityConvertInto, CapabilityConvertFrom, CapabilityListed,
}

// StateCapabilities returns the capabilities of a state, before overrides.
//
//   - active and delist_announced allow everything;
//   - deposits_disabled refuses deposits;
//   - sell_only also refuses buying and converting into the currency;
//   - trading_halted and withdrawal_only allow only withdrawals;
//   - frozen allows nothing;
//   - delisted allows nothing and leaves the public lists.
func StateCapabilities(l Lifecycle) models.CurrencyCapabilities {
	all := models.CurrencyCapabilities{Deposit: true, Withdraw: true, Buy: true, Sell: true, ConvertInto: true, ConvertFrom: true, Listed: true}
	switch l {
	case LifecycleActive, LifecycleDelistAnnounced:
		return all
	case LifecycleDepositsDisabled:
		all.Deposit = false
		return all
	case LifecycleSellOnly:
		all.Deposit, all.Buy, all.ConvertInto = false, false, false
		return all
	case LifecycleTradingHalted, LifecycleWithdrawalOnly:
		return models.CurrencyCapabilities{Withdraw: true, Listed: true}
	case LifecycleFrozen:
		return models.CurrencyCapabilities{Listed: true}
	default:
		return models.CurrencyCapabilities{}
	}
}

// capability returns the field of c that holds capability k.
func capability(c *models.CurrencyCapabilities, k Capability) *bool {
	switch k {
	case CapabilityDeposit:
		return &c.Deposit
	case CapabilityWithdraw:
		return &c.Withdraw
	case CapabilityBuy:
		return &c.Buy
	case CapabilitySell:
		return &c.Sell
	case CapabilityConvertInto:
		return &c.ConvertInto
	case CapabilityConvertFrom:
		return &c.ConvertFrom
	case CapabilityListed:
		return &c.Listed
	}
	return nil
}

// Allows reports whether the capabilities allow k.
func Allows(c models.CurrencyCapabilities, k Capability) bool {
	p := capability(&c, k)
	return p != nil && *p
}

// LifecycleOf returns the lifecycle of a state with its overrides, keyed by
// capability.
func LifecycleOf(l Lifecycle, overrides map[Capability]bool, next *models.CurrencyLifecycleStep) *models.CurrencyLifecycle {
	caps := StateCapabilities(l)
	for k, allowed := range overrides {
		if p := capability(&caps, k); p != nil {
			*p = allowed
		}
	}
	return &models.CurrencyLifecycle{State: string(l), Capabilities: caps, Next: next}
}

// Listed reports whether a currency is in the public lists. A currency read
// from a cache of an earlier version has no lifecycle, and is listed.
func Listed(c models.Currency) bool {
	return c.Lifecycle == nil || c.Lifecycle.Capabilities.Listed
}

// LifecycleColumns are the columns that ScanLifecycle reads, for a query of
// the currencies table under the alias c.
const LifecycleColumns = `c.lifecycle,
	COALESCE((SELECT json_object_agg(o.capability, o.allowed) FROM currency_capability_overrides o
		WHERE o.currency_hk = c.hk), '{}'::json),
	(SELECT json_build_object('state', s.state, 'at', s.scheduled_at)
		FROM currency_delisting_steps s JOIN currency_delistings d ON d.hk = s.delisting_hk
		WHERE d.currency_hk = c.hk AND d.status = 'scheduled' AND s.applied_at IS NULL
		ORDER BY s.scheduled_at LIMIT 1)`

// LifecycleScan holds the LifecycleColumns of a row, for Scan.
type LifecycleScan struct {
	state     string
	overrides []byte
	next      []byte
}

// Dest returns the scan destinations of the LifecycleColumns.
func (s *LifecycleScan) Dest() []any { return []any{&s.state, &s.overrides, &s.next} }

// Lifecycle decodes the scanned columns.
func (s *LifecycleScan) Lifecycle() (*models.CurrencyLifecycle, error) {
	overrides := map[Capability]bool{}
	if err := json.Unmarshal(s.overrides, &overrides); err != nil {
		return nil, fmt.Errorf("invalid capability overrides: %w", err)
	}
	var next *models.CurrencyLifecycleStep
	if len(s.next) > 0 {
		next = &models.CurrencyLifecycleStep{}
		if err := json.Unmarshal(s.next, next); err != nil {
			return nil, fmt.Errorf("invalid next step: %w", err)
		}
	}
	return LifecycleOf(Lifecycle(s.state), overrides, next), nil
}

// Querier runs a query, on a database or in a transaction.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ErrNotFound is a currency that does not exist or is deleted.
var ErrNotFound = errors.New("currency not found")

// ReadLifecycle returns the lifecycle of a currency. With lock, in a
// transaction, it also takes a share lock of the currency, so that a step of
// a delisting, which takes an update lock, waits for the transaction: a
// flow that checked a capability finishes before the capability changes.
func ReadLifecycle(ctx context.Context, q Querier, currencyHK string, lock bool) (*models.CurrencyLifecycle, error) {
	query := `SELECT ` + LifecycleColumns + ` FROM currencies c WHERE c.hk = $1 AND c.deleted_at IS NULL`
	if lock {
		query += ` FOR SHARE OF c`
	}
	var s LifecycleScan
	err := q.QueryRowContext(ctx, query, currencyHK).Scan(s.Dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the lifecycle of the currency: %w", err)
	}
	return s.Lifecycle()
}

// CodeCurrencyOperationDisabled is the error code of a refusal by a
// capability of a currency.
const CodeCurrencyOperationDisabled = "currency_operation_disabled"

// Refusal is an operation that the lifecycle of a currency does not allow.
// It is an error.
type Refusal struct {
	CurrencyHK string
	Capability Capability
	Lifecycle  *models.CurrencyLifecycle
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("%s: %s of %s at %s", CodeCurrencyOperationDisabled, r.Capability, r.CurrencyHK, r.Lifecycle.State)
}

// Check returns a *Refusal when the currency does not allow k now, and
// ErrNotFound when there is no such currency. A check that guards a change
// of money runs in its transaction, with lock.
func Check(ctx context.Context, q Querier, currencyHK string, k Capability, lock bool) error {
	if !slices.Contains(Capabilities, k) {
		return fmt.Errorf("unknown capability %q", k)
	}
	l, err := ReadLifecycle(ctx, q, currencyHK, lock)
	if err != nil {
		return err
	}
	if !Allows(l.Capabilities, k) {
		return &Refusal{CurrencyHK: currencyHK, Capability: k, Lifecycle: l}
	}
	return nil
}

// SendRefusal answers a refusal with 403 and its code, with the state of
// the currency and its next step, so that clients can say when and why. It
// reports false, and sends nothing, for an err that is not a *Refusal.
func SendRefusal(c *gin.Context, err error) bool {
	var r *Refusal
	if !errors.As(err, &r) {
		return false
	}
	info := gin.H{"currency_hk": r.CurrencyHK, "capability": r.Capability, "lifecycle": r.Lifecycle.State}
	message := "This operation is not available for this currency now."
	switch Lifecycle(r.Lifecycle.State) {
	case LifecycleActive:
	case LifecycleFrozen, LifecycleDelisted:
		message = "This currency is delisted, and no operation is available for it."
	default:
		message = "This currency is being delisted, and this operation is no longer available for it."
	}
	helpers.SendError(c, http.StatusForbidden, CodeCurrencyOperationDisabled, message, info)
	return true
}
