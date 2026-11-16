// Package venues is the authenticated side of the external exchanges
// (#384, epic #383): a Venue is what the router of OTC trades (#386, #387)
// talks to, to read our balances at an exchange, place and look up orders,
// and move funds out and between its accounts. The market data service
// (#262) reads only their public data, through internal/marketdata/source.
//
// Each connector maps the venue's own API to this interface, in the venue's
// own symbols and units. Which of our currencies and networks a venue's
// asset is, is configured per venue (Mapping), never guessed from a symbol.
//
// Every call that changes something carries an id that we choose (a client
// order id, a withdrawal id, a transfer id), so that after a crash, or an
// answer lost in a timeout, the caller looks the order up by that id
// instead of placing it twice. See docs/venues.md.
package venues

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// Mode is how a venue is used.
type Mode string

const (
	// ModeTestnet calls the venue's test environment, where it has one.
	ModeTestnet Mode = "testnet"
	// ModeLive calls the venue with real funds.
	ModeLive Mode = "live"
	// ModeDryRun reads the venue, and logs the orders, withdrawals, and
	// transfers without sending them.
	ModeDryRun Mode = "dry_run"
)

// Modes are the modes, in order.
var Modes = []Mode{ModeTestnet, ModeLive, ModeDryRun}

// Capability is something a venue may not offer, or not in every mode.
type Capability string

const (
	CapabilityWithdraw         Capability = "withdraw"
	CapabilityDepositAddress   Capability = "deposit_address"
	CapabilityNetworks         Capability = "networks"
	CapabilityInternalTransfer Capability = "internal_transfer"
	CapabilityWhitelist        Capability = "withdrawal_whitelist"
)

// Side is the side of an order.
type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

// OrderType is the type of an order.
type OrderType string

const (
	OrderLimit  OrderType = "limit"
	OrderMarket OrderType = "market"
)

// OrderStatus is the state of an order at a venue.
type OrderStatus string

const (
	OrderNew             OrderStatus = "new"
	OrderPartiallyFilled OrderStatus = "partially_filled"
	OrderFilled          OrderStatus = "filled"
	OrderCancelled       OrderStatus = "cancelled"
	OrderRejected        OrderStatus = "rejected"
	OrderExpired         OrderStatus = "expired"
)

// Done reports whether the order can change no more.
func (s OrderStatus) Done() bool {
	return s == OrderFilled || s == OrderCancelled || s == OrderRejected || s == OrderExpired
}

// TransferStatus is the state of a withdrawal or a transfer.
type TransferStatus string

const (
	TransferPending   TransferStatus = "pending"
	TransferCompleted TransferStatus = "completed"
	TransferFailed    TransferStatus = "failed"
	TransferCancelled TransferStatus = "cancelled"
)

// Balance is what we hold of one asset at a venue.
type Balance struct {
	Asset  string         `json:"asset"`
	Free   shared.Decimal `json:"free"`
	Locked shared.Decimal `json:"locked"`
}

// Instrument is a market of a venue.
type Instrument struct {
	// Symbol is the venue's name of the market, such as BTCUSDT or btc-rls.
	Symbol string `json:"symbol"`
	Base   string `json:"base"`
	Quote  string `json:"quote"`
	// TickSize is the step of the price, LotSize the step of the quantity,
	// and MinQuantity and MinNotional the smallest order; a zero step or
	// minimum is none.
	TickSize    shared.Decimal `json:"tick_size"`
	LotSize     shared.Decimal `json:"lot_size"`
	MinQuantity shared.Decimal `json:"min_quantity"`
	MinNotional shared.Decimal `json:"min_notional"`
	// Trading is false while the venue halts the market.
	Trading bool `json:"trading"`
}

// Network is a network of an asset at a venue.
type Network struct {
	// Network is the venue's name of the network, such as TRX or BSC.
	Network          string         `json:"network"`
	DepositEnabled   bool           `json:"deposit_enabled"`
	WithdrawEnabled  bool           `json:"withdraw_enabled"`
	WithdrawFee      shared.Decimal `json:"withdraw_fee"`
	WithdrawMin      shared.Decimal `json:"withdraw_min"`
	Confirmations    int            `json:"confirmations"`
	AddressTagNeeded bool           `json:"address_tag_needed"`
}

// OrderRequest is an order to place.
type OrderRequest struct {
	// ClientID is our id of the order, unique per venue: GetOrder finds the
	// order by it, also after a crash.
	ClientID string
	Symbol   string
	Side     Side
	Type     OrderType
	Quantity shared.Decimal
	// Price is the limit of a limit order.
	Price shared.Decimal
}

// Order is an order at a venue.
type Order struct {
	ClientID string         `json:"client_id"`
	VenueID  string         `json:"venue_id"`
	Symbol   string         `json:"symbol"`
	Side     Side           `json:"side"`
	Type     OrderType      `json:"type"`
	Status   OrderStatus    `json:"status"`
	Quantity shared.Decimal `json:"quantity"`
	Price    shared.Decimal `json:"price"`
	// Filled is the quantity filled, and FilledQuote what it cost or
	// brought, in the quote asset.
	Filled      shared.Decimal `json:"filled"`
	FilledQuote shared.Decimal `json:"filled_quote"`
	UpdatedAt   time.Time      `json:"updated_at"`
	// DryRun marks an order of ModeDryRun, which was never sent.
	DryRun bool `json:"dry_run,omitempty"`
}

// WithdrawRequest is a withdrawal to send.
type WithdrawRequest struct {
	// ClientID is our id of the withdrawal; GetWithdrawal finds it by it.
	ClientID string
	Asset    string
	Network  string
	Address  string
	// Tag is the memo or tag of networks that need one.
	Tag    string
	Amount shared.Decimal
}

// Withdrawal is a withdrawal from a venue.
type Withdrawal struct {
	ClientID string         `json:"client_id"`
	VenueID  string         `json:"venue_id"`
	Asset    string         `json:"asset"`
	Network  string         `json:"network"`
	Amount   shared.Decimal `json:"amount"`
	Fee      shared.Decimal `json:"fee"`
	Status   TransferStatus `json:"status"`
	TxID     string         `json:"tx_id,omitempty"`
	DryRun   bool           `json:"dry_run,omitempty"`
}

// DepositAddress is where to deposit an asset at a venue.
type DepositAddress struct {
	Asset   string `json:"asset"`
	Network string `json:"network"`
	Address string `json:"address"`
	Tag     string `json:"tag,omitempty"`
}

// TransferRequest moves an asset between the main account of a venue and
// one of its sub-accounts; an empty From or To is the main account.
type TransferRequest struct {
	ClientID string
	Asset    string
	Amount   shared.Decimal
	From, To string
}

// Transfer is a transfer between the accounts of a venue.
type Transfer struct {
	ClientID string         `json:"client_id"`
	VenueID  string         `json:"venue_id"`
	Asset    string         `json:"asset"`
	Amount   shared.Decimal `json:"amount"`
	Status   TransferStatus `json:"status"`
	DryRun   bool           `json:"dry_run,omitempty"`
}

// Fee is a fee that a venue charged us apart from its fills, such as the
// fee of a withdrawal, as it reports it.
type Fee struct {
	// ID is the venue's id of the fee, unique per venue: the fee is posted
	// once per id.
	ID     string
	Asset  string
	Amount shared.Decimal
}

// FeeReporter is a venue that reports the fees it charged apart from its
// fills (#503). The reconciliation (#385) posts them before it compares
// the balances; a venue that reports none has every such fee show as a
// difference.
type FeeReporter interface {
	// Fees returns the fees charged since since, or a window ending now
	// that the venue bounds. A fee may be returned again: it is posted
	// once per ID.
	Fees(ctx context.Context, since time.Time) ([]Fee, error)
}

// Venue is an external exchange, through our account at it. Every method
// is safe for concurrent use. A capability that the venue, or its mode,
// does not offer answers ErrUnsupported; Supports says so beforehand.
type Venue interface {
	// Name is the venue's name in the configuration, such as binance.
	Name() string
	Mode() Mode
	Supports(Capability) bool

	Balances(ctx context.Context) ([]Balance, error)
	Instruments(ctx context.Context) ([]Instrument, error)
	Networks(ctx context.Context, asset string) ([]Network, error)

	PlaceOrder(ctx context.Context, req OrderRequest) (Order, error)
	CancelOrder(ctx context.Context, symbol, clientID string) (Order, error)
	// GetOrder finds an order by our id; ErrNotFound when the venue has
	// none, so that a crash before the order was sent is told from one
	// after.
	GetOrder(ctx context.Context, symbol, clientID string) (Order, error)

	Withdraw(ctx context.Context, req WithdrawRequest) (Withdrawal, error)
	GetWithdrawal(ctx context.Context, clientID string) (Withdrawal, error)
	DepositAddress(ctx context.Context, asset, network string) (DepositAddress, error)

	InternalTransfer(ctx context.Context, req TransferRequest) (Transfer, error)
	GetTransfer(ctx context.Context, clientID string) (Transfer, error)
	// Whitelisted reports whether address is on the withdrawal whitelist of
	// our account for the asset and network.
	Whitelisted(ctx context.Context, asset, network, address, tag string) (bool, error)
}

// Kind is how a caller treats an error of a venue.
type Kind string

const (
	// KindRetryable failed without effect: the same call may be tried
	// again, after a wait.
	KindRetryable Kind = "retryable"
	// KindRejected was refused by the venue, such as for funds, a price
	// out of its filters, or a bad address: trying again does not help.
	KindRejected Kind = "rejected"
	// KindUnknown may have taken effect: the caller looks it up by its
	// client id before anything else.
	KindUnknown Kind = "unknown_outcome"
)

// CodeDuplicate is the Code of a PlaceOrder whose client id the venue
// already holds: the caller looks the order up with GetOrder. A venue may
// know only its open orders by client id, so a caller never places again
// before it looked the id up.
const CodeDuplicate = "duplicate_client_id"

// Errors that are not of a call.
var (
	ErrUnsupported = errors.New("venues: not supported by this venue or mode")
	ErrNotFound    = errors.New("venues: not found")
)

// Error is an error of a call to a venue. It never holds a credential, a
// signature, or a signed URL.
type Error struct {
	Venue string
	Op    string
	Kind  Kind
	// Code is the venue's own code of the error, when it gives one.
	Code string
	Err  error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("venue %s: %s: %s", e.Venue, e.Op, e.Kind)
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf returns the kind of err: of an *Error, or retryable for any other
// error, such as a context that ended before the call.
func KindOf(err error) Kind {
	var ve *Error
	if errors.As(err, &ve) {
		return ve.Kind
	}
	return KindRetryable
}
