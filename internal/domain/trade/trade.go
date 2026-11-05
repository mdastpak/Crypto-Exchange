package trade

import (
	"encrypted-db/internal/domain/market"
	"encrypted-db/internal/domain/order"
	"encrypted-db/internal/domain/shared"
)

// Trade represents an executed trade (fill)
type Trade struct {
	ID shared.UUID `json:"id" db:"id"`

	// Order references
	MakerOrderID shared.UUID `json:"maker_order_id" db:"maker_order_id"`
	TakerOrderID shared.UUID `json:"taker_order_id" db:"taker_order_id"`

	// Market
	MarketID shared.UUID `json:"market_id" db:"market_id"`

	// Trade details
	Side     shared.TradeSide `json:"side" db:"side"`         // Taker side (aggressive)
	Price    shared.Decimal   `json:"price" db:"price"`       // Execution price
	Quantity shared.Decimal   `json:"quantity" db:"quantity"` // Executed quantity

	// Fees
	MakerFee        shared.Decimal `json:"maker_fee" db:"maker_fee"`
	MakerFeeAssetID shared.UUID    `json:"maker_fee_asset_id" db:"maker_fee_asset_id"`
	MakerFeeRateBPS int            `json:"maker_fee_rate_bps" db:"maker_fee_rate_bps"`

	TakerFee        shared.Decimal `json:"taker_fee" db:"taker_fee"`
	TakerFeeAssetID shared.UUID    `json:"taker_fee_asset_id" db:"taker_fee_asset_id"`
	TakerFeeRateBPS int            `json:"taker_fee_rate_bps" db:"taker_fee_rate_bps"`

	// Settlement
	SettlementMode shared.SettlementMode `json:"settlement_mode" db:"settlement_mode"`
	Settled        bool                  `json:"settled" db:"settled"`
	SettledAt      *shared.Timestamp     `json:"settled_at,omitempty" db:"settled_at"`

	// Microsecond timestamp for ordering
	Timestamp shared.Timestamp `json:"timestamp" db:"timestamp"`

	// Relations (populated on join)
	Market     *market.Market `json:"market,omitempty" db:"-"`
	MakerOrder *order.Order   `json:"maker_order,omitempty" db:"-"`
	TakerOrder *order.Order   `json:"taker_order,omitempty" db:"-"`
}

// Notional returns the quote asset value of the trade
func (t *Trade) Notional() shared.Decimal {
	return t.Price.Mul(t.Quantity)
}

// TotalFees returns total fees collected
func (t *Trade) TotalFees() shared.Decimal {
	return t.MakerFee.Add(t.TakerFee)
}

// IsMaker returns true if the given order ID is the maker
func (t *Trade) IsMaker(orderID shared.UUID) bool {
	return t.MakerOrderID == orderID
}

// IsTaker returns true if the given order ID is the taker
func (t *Trade) IsTaker(orderID shared.UUID) bool {
	return t.TakerOrderID == orderID
}

// GetFeeForOrder returns the fee paid by a specific order
func (t *Trade) GetFeeForOrder(orderID shared.UUID) (shared.Decimal, shared.UUID) {
	if t.IsMaker(orderID) {
		return t.MakerFee, t.MakerFeeAssetID
	}
	if t.IsTaker(orderID) {
		return t.TakerFee, t.TakerFeeAssetID
	}
	return shared.Decimal{}, shared.UUID{}
}

// TradeEvent represents a trade execution event for event sourcing
type TradeEvent struct {
	EventID   shared.UUID      `json:"event_id"`
	TradeID   shared.UUID      `json:"trade_id"`
	EventType TradeEventType   `json:"event_type"`
	Timestamp shared.Timestamp `json:"timestamp"`

	// Trade data
	MarketID shared.UUID      `json:"market_id"`
	Side     shared.TradeSide `json:"side"`
	Price    shared.Decimal   `json:"price"`
	Quantity shared.Decimal   `json:"quantity"`

	// Parties
	MakerOrderID shared.UUID `json:"maker_order_id"`
	TakerOrderID shared.UUID `json:"taker_order_id"`
	MakerUserID  shared.UUID `json:"maker_user_id"`
	TakerUserID  shared.UUID `json:"taker_user_id"`

	// Fees
	MakerFee shared.Decimal `json:"maker_fee"`
	TakerFee shared.Decimal `json:"taker_fee"`

	// Settlement
	SettlementMode shared.SettlementMode `json:"settlement_mode"`
}

type TradeEventType string

const (
	TradeEventExecuted TradeEventType = "TRADE_EXECUTED"
	TradeEventSettled  TradeEventType = "TRADE_SETTLED"
)

func (t TradeEventType) String() string {
	return string(t)
}

// TradeAggregate represents aggregated trade data for analytics
type TradeAggregate struct {
	MarketID  shared.UUID      `json:"market_id"`
	Interval  string           `json:"interval"` // 1m, 5m, 1h, 1d
	StartTime shared.Timestamp `json:"start_time"`
	EndTime   shared.Timestamp `json:"end_time"`

	// OHLCV
	OpenPrice   shared.Decimal `json:"open_price"`
	HighPrice   shared.Decimal `json:"high_price"`
	LowPrice    shared.Decimal `json:"low_price"`
	ClosePrice  shared.Decimal `json:"close_price"`
	BaseVolume  shared.Decimal `json:"base_volume"`
	QuoteVolume shared.Decimal `json:"quote_volume"`
	TradeCount  int64          `json:"trade_count"`

	// VWAP
	VWAP shared.Decimal `json:"vwap"`

	// Buy/Sell volume
	BuyVolume  shared.Decimal `json:"buy_volume"`
	SellVolume shared.Decimal `json:"sell_volume"`
	BuyCount   int64          `json:"buy_count"`
	SellCount  int64          `json:"sell_count"`
}
