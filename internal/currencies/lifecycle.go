package currencies

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/helpers"
	"github.com/mdastpak/crypto-exchange/internal/i18n"
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

// Listed reports whether a currency is in the public lists. A currency
// without a lifecycle is listed.
func Listed(c models.Currency) bool {
	return c.Lifecycle == nil || c.Lifecycle.Capabilities.Listed
}

// DetailColumns are the columns of a currency beyond its row, which
// DetailScan reads, for a query of the currencies table under the alias c:
// the lifecycle with its overrides and next step, and the previous symbols.
const DetailColumns = `c.lifecycle,
	COALESCE((SELECT json_object_agg(o.capability, o.allowed) FROM currency_capability_overrides o
		WHERE o.currency_hk = c.hk AND o.cleared_at IS NULL), '{}'::json),
	(SELECT json_build_object('state', s.state, 'at', s.scheduled_at)
		FROM currency_delisting_steps s JOIN currency_delistings d ON d.hk = s.delisting_hk
		WHERE d.currency_hk = c.hk AND d.status = 'scheduled' AND s.applied_at IS NULL
		ORDER BY s.scheduled_at LIMIT 1),
	COALESCE((SELECT string_agg(p.symbol, ',' ORDER BY p.created_at DESC, p.symbol) FROM currency_symbols p
		WHERE p.currency_hk = c.hk AND NOT p.current AND p.released_at IS NULL), ''),
	(SELECT json_build_object('id', m.hk, 'target_hk', m.target_hk, 'phase', m.phase, 'forced', m.forced,
			'ratio', trim_scale(src.ratio)::TEXT,
			'next', (SELECT json_build_object('phase', ph.phase, 'at', ph.scheduled_at) FROM currency_migration_phases ph
				WHERE ph.migration_hk = m.hk AND ph.applied_at IS NULL ORDER BY ph.scheduled_at LIMIT 1))
		FROM currency_migration_sources src JOIN currency_migrations m ON m.hk = src.migration_hk
		WHERE src.currency_hk = c.hk AND m.phase NOT IN ('scheduled', 'cancelled')
		ORDER BY m.created_at DESC LIMIT 1)`

// DetailScan holds the DetailColumns of a row, for Scan.
type DetailScan struct {
	state     string
	overrides []byte
	next      []byte
	previous  string
	migration []byte
}

// Dest returns the scan destinations of the DetailColumns.
func (s *DetailScan) Dest() []any {
	return []any{&s.state, &s.overrides, &s.next, &s.previous, &s.migration}
}

// Apply sets the details of the scanned row on c.
func (s *DetailScan) Apply(c *models.Currency) error {
	l, err := s.Lifecycle()
	if err != nil {
		return err
	}
	c.Lifecycle = l
	c.Migration, err = s.Migration()
	if err != nil {
		return err
	}
	c.PreviousSymbols = nil
	if s.previous != "" {
		c.PreviousSymbols = strings.Split(s.previous, ",")
	}
	return nil
}

// Migration decodes the migration of the scanned row, nil when it is the
// source of none.
func (s *DetailScan) Migration() (*models.CurrencyMigration, error) {
	if len(s.migration) == 0 {
		return nil, nil
	}
	var m models.CurrencyMigration
	if err := json.Unmarshal(s.migration, &m); err != nil {
		return nil, fmt.Errorf("invalid migration: %w", err)
	}
	return &m, nil
}

// Lifecycle decodes the scanned columns: the capabilities of the lifecycle
// state, narrowed by the phase of a migration, with the overrides.
func (s *DetailScan) Lifecycle() (*models.CurrencyLifecycle, error) {
	m, err := s.Migration()
	if err != nil {
		return nil, err
	}
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
	l := LifecycleOf(Lifecycle(s.state), nil, next)
	if m != nil {
		l.Capabilities = narrow(l.Capabilities, MigrationCapabilities(m.Phase, m.Forced))
	}
	for k, allowed := range overrides {
		if p := capability(&l.Capabilities, k); p != nil {
			*p = allowed
		}
	}
	return l, nil
}

// narrow allows what both a and b allow.
func narrow(a, b models.CurrencyCapabilities) models.CurrencyCapabilities {
	for _, k := range Capabilities {
		if p := capability(&a, k); p != nil {
			*p = *p && Allows(b, k)
		}
	}
	return a
}

// MigrationCapabilities returns what the source of a migration allows in a
// phase. A forced migration pauses deposits and withdrawals of its sources,
// then halts their trading and conversions, and retires them from the lists
// once completed. An optional one changes nothing: the source stays, and the
// conversion is offered.
func MigrationCapabilities(phase string, forced bool) models.CurrencyCapabilities {
	all := StateCapabilities(LifecycleActive)
	if !forced {
		return all
	}
	switch phase {
	case "announced":
		return all
	case "deposits_paused":
		all.Deposit, all.Withdraw = false, false
		return all
	case "trading_halted", "converting":
		return models.CurrencyCapabilities{Listed: true}
	default:
		return models.CurrencyCapabilities{}
	}
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
	var c models.Currency
	if err := readDetails(ctx, q, currencyHK, lock, &c); err != nil {
		return nil, err
	}
	return c.Lifecycle, nil
}

func readDetails(ctx context.Context, q Querier, currencyHK string, lock bool, c *models.Currency) error {
	query := `SELECT COALESCE(c.info->>'symbol', ''), ` + DetailColumns + ` FROM currencies c WHERE c.hk = $1 AND c.deleted_at IS NULL`
	if lock {
		query += ` FOR SHARE OF c`
	}
	var s DetailScan
	err := q.QueryRowContext(ctx, query, currencyHK).Scan(append([]any{&c.Info.Symbol}, s.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to read the lifecycle of the currency: %w", err)
	}
	return s.Apply(c)
}

// CodeCurrencyOperationDisabled is the error code of a refusal by a
// capability of a currency.
const CodeCurrencyOperationDisabled = "currency_operation_disabled"

// Refusal is an operation that the lifecycle of a currency does not allow.
// It is an error.
type Refusal struct {
	CurrencyHK string
	// Symbol is the symbol of the currency, for the message.
	Symbol     string
	Capability Capability
	Lifecycle  *models.CurrencyLifecycle
	// Migration is the migration of the currency, when it has one.
	Migration *models.CurrencyMigration
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
	var c models.Currency
	if err := readDetails(ctx, q, currencyHK, lock, &c); err != nil {
		return err
	}
	if !Allows(c.Lifecycle.Capabilities, k) {
		return &Refusal{CurrencyHK: currencyHK, Symbol: c.Info.Symbol, Capability: k, Lifecycle: c.Lifecycle, Migration: c.Migration}
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
	operation := i18n.Key("CAPABILITY_" + strings.ToUpper(string(r.Capability)))
	message := i18n.M("CURRENCY_OPERATION_UNAVAILABLE", i18n.A("operation", operation), i18n.A("currency", r.Symbol))
	if r.Migration != nil {
		info["migration"] = gin.H{"id": r.Migration.ID, "target_hk": r.Migration.TargetHK, "phase": r.Migration.Phase}
	}
	switch Lifecycle(r.Lifecycle.State) {
	case LifecycleActive:
		if r.Migration != nil && r.Migration.Forced {
			message = i18n.M("CURRENCY_OPERATION_PAUSED_BY_MIGRATION", i18n.A("operation", operation), i18n.A("currency", r.Symbol))
		}
	case LifecycleFrozen, LifecycleDelisted:
		message = i18n.M("CURRENCY_DELISTED", i18n.A("currency", r.Symbol))
	default:
		message = i18n.M("CURRENCY_BEING_DELISTED", i18n.A("operation", operation), i18n.A("currency", r.Symbol))
	}
	helpers.SendError(c, http.StatusForbidden, CodeCurrencyOperationDisabled, message, info)
	return true
}

// ValidDecimal reports whether s is a plain positive-or-zero decimal number,
// as NUMERIC(38, 18) holds it: digits, with at most one point and at most
// 18 digits after it, and at most 20 before.
func ValidDecimal(s string) bool {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || len(whole) > 20 || len(frac) > 18 || strings.Contains(s, ".") && frac == "" {
		return false
	}
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
