package order

import (
	"github.com/mdastpak/crypto-exchange/internal/domain/market"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// Order represents a trading order
type Order struct {
	ID            shared.UUID `json:"id" db:"id"`
	ClientOrderID string      `json:"client_order_id,omitempty" db:"client_order_id"` // User-provided ID

	// Ownership
	UserID       shared.UUID  `json:"user_id" db:"user_id"`
	SubAccountID *shared.UUID `json:"sub_account_id,omitempty" db:"sub_account_id"`

	// Market
	MarketID shared.UUID `json:"market_id" db:"market_id"`

	// Order details
	Side        shared.OrderSide   `json:"side" db:"side"`
	Type        shared.OrderType   `json:"type" db:"type"`
	TimeInForce shared.TimeInForce `json:"time_in_force" db:"time_in_force"`

	// Price & quantity
	Price          shared.Decimal `json:"price" db:"price"`                     // Limit price
	StopPrice      shared.Decimal `json:"stop_price,omitempty" db:"stop_price"` // For stop orders
	Quantity       shared.Decimal `json:"quantity" db:"quantity"`               // Original quantity
	FilledQuantity shared.Decimal `json:"filled_quantity" db:"filled_quantity"` // Filled so far

	// Status
	Status       shared.OrderStatus `json:"status" db:"status"`
	RejectReason string             `json:"reject_reason,omitempty" db:"reject_reason"`

	// Fees
	FeeAssetID shared.UUID    `json:"fee_asset_id" db:"fee_asset_id"` // Asset to pay fees in
	FeePaid    shared.Decimal `json:"fee_paid" db:"fee_paid"`         // Accumulated fees
	FeeRateBPS int            `json:"fee_rate_bps" db:"fee_rate_bps"` // Applied fee rate

	// Risk
	ReduceOnly bool `json:"reduce_only" db:"reduce_only"` // For position reduction only
	PostOnly   bool `json:"post_only" db:"post_only"`     // Post-only (maker only)

	// Timestamps
	CreatedAt   shared.Timestamp  `json:"created_at" db:"created_at"`
	UpdatedAt   shared.Timestamp  `json:"updated_at" db:"updated_at"`
	ExpiredAt   *shared.Timestamp `json:"expired_at,omitempty" db:"expired_at"`
	CancelledAt *shared.Timestamp `json:"cancelled_at,omitempty" db:"cancelled_at"`

	// Relations (populated on join)
	Market *market.Market `json:"market,omitempty" db:"-"`
}

// RemainingQuantity returns unfilled quantity
func (o *Order) RemainingQuantity() shared.Decimal {
	return o.Quantity.Sub(o.FilledQuantity)
}

// IsActive checks if order can still be filled
func (o *Order) IsActive() bool {
	switch o.Status {
	case shared.OrderStatusNew, shared.OrderStatusPartiallyFilled:
		return true
	default:
		return false
	}
}

// IsTerminal checks if order is in final state
func (o *Order) IsTerminal() bool {
	switch o.Status {
	case shared.OrderStatusFilled, shared.OrderStatusCancelled,
		shared.OrderStatusRejected, shared.OrderStatusExpired:
		return true
	default:
		return false
	}
}

// FillPercentage returns fill progress as percentage (0-100)
func (o *Order) FillPercentage() float64 {
	if o.Quantity.IsZero() {
		return 0
	}
	hundred := shared.NewDecimalFromInt64(100)
	return o.FilledQuantity.Div(o.Quantity).Mul(hundred).InexactFloat64()
}

// CanCancel checks if order can be cancelled
func (o *Order) CanCancel() bool {
	return o.IsActive()
}

// OrderEvent represents an order lifecycle event for event sourcing
type OrderEvent struct {
	EventID   shared.UUID      `json:"event_id"`
	OrderID   shared.UUID      `json:"order_id"`
	EventType OrderEventType   `json:"event_type"`
	Timestamp shared.Timestamp `json:"timestamp"`

	// State changes
	OldStatus shared.OrderStatus `json:"old_status,omitempty"`
	NewStatus shared.OrderStatus `json:"new_status,omitempty"`
	OldFilled shared.Decimal     `json:"old_filled,omitempty"`
	NewFilled shared.Decimal     `json:"new_filled,omitempty"`

	// Trade details (for FILL events)
	TradeID    *shared.UUID    `json:"trade_id,omitempty"`
	TradePrice *shared.Decimal `json:"trade_price,omitempty"`
	TradeQty   *shared.Decimal `json:"trade_qty,omitempty"`
	TradeFee   *shared.Decimal `json:"trade_fee,omitempty"`

	// Metadata
	Metadata map[string]string `json:"metadata,omitempty"`
}

type OrderEventType string

const (
	OrderEventCreated         OrderEventType = "ORDER_CREATED"
	OrderEventPartiallyFilled OrderEventType = "ORDER_PARTIALLY_FILLED"
	OrderEventFilled          OrderEventType = "ORDER_FILLED"
	OrderEventCancelled       OrderEventType = "ORDER_CANCELLED"
	OrderEventRejected        OrderEventType = "ORDER_REJECTED"
	OrderEventExpired         OrderEventType = "ORDER_EXPIRED"
	OrderEventModified        OrderEventType = "ORDER_MODIFIED"
)

func (o OrderEventType) String() string {
	return string(o)
}

// OrderBookSnapshot represents a point-in-time order book state
type OrderBookSnapshot struct {
	MarketID  shared.UUID      `json:"market_id"`
	Timestamp shared.Timestamp `json:"timestamp"`
	Sequence  int64            `json:"sequence"`
	Bids      []OrderBookLevel `json:"bids"`
	Asks      []OrderBookLevel `json:"asks"`
}

type OrderBookLevel struct {
	Price      shared.Decimal `json:"price"`
	Quantity   shared.Decimal `json:"quantity"`
	OrderCount int            `json:"order_count"`
}
