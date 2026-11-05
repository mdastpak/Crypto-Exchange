package market

import (
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/asset"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// Market represents a trading market (pair of assets with trading rules)
type Market struct {
	ID           shared.UUID `json:"id" db:"id"`
	BaseAssetID  shared.UUID `json:"base_asset_id" db:"base_asset_id"`
	QuoteAssetID shared.UUID `json:"quote_asset_id" db:"quote_asset_id"`

	// Trading parameters
	Status       MarketStatus   `json:"status" db:"status"`
	MinOrderSize shared.Decimal `json:"min_order_size" db:"min_order_size"` // Minimum base asset quantity
	MaxOrderSize shared.Decimal `json:"max_order_size" db:"max_order_size"` // Maximum base asset quantity
	MinNotional  shared.Decimal `json:"min_notional" db:"min_notional"`     // Minimum quote asset value
	TickSize     shared.Decimal `json:"tick_size" db:"tick_size"`           // Price increment (e.g., 0.01)
	LotSize      shared.Decimal `json:"lot_size" db:"lot_size"`             // Quantity increment (e.g., 0.001)

	// Fee structure (basis points)
	MakerFeeBPS int `json:"maker_fee_bps" db:"maker_fee_bps"` // e.g., 10 = 0.10%
	TakerFeeBPS int `json:"taker_fee_bps" db:"taker_fee_bps"` // e.g., 20 = 0.20%

	// Risk limits
	MaxPositionSize shared.Decimal `json:"max_position_size" db:"max_position_size"` // Per user
	MaxLeverage     int            `json:"max_leverage" db:"max_leverage"`           // For margin (1 = spot)

	// Market making
	MarketMakerFeeBPS int `json:"market_maker_fee_bps" db:"market_maker_fee_bps"`

	// Price bands (for circuit breakers)
	PriceBandBPS int `json:"price_band_bps" db:"price_band_bps"` // Max deviation from mark price

	// 24h statistics (updated periodically)
	Stats *MarketStats `json:"stats,omitempty" db:"-"`

	// Relations (populated on join)
	BaseAsset  *asset.Asset `json:"base_asset,omitempty" db:"-"`
	QuoteAsset *asset.Asset `json:"quote_asset,omitempty" db:"-"`

	CreatedAt shared.Timestamp  `json:"created_at" db:"created_at"`
	UpdatedAt shared.Timestamp  `json:"updated_at" db:"updated_at"`
	DeletedAt *shared.Timestamp `json:"deleted_at,omitempty" db:"deleted_at"`
}

type MarketStatus string

const (
	MarketStatusActive      MarketStatus = "ACTIVE"
	MarketStatusInactive    MarketStatus = "INACTIVE"
	MarketStatusMaintenance MarketStatus = "MAINTENANCE"
	MarketStatusSuspended   MarketStatus = "SUSPENDED"
	MarketStatusDelisted    MarketStatus = "DELISTED"
)

func (m MarketStatus) String() string {
	return string(m)
}

// MarketStats holds 24h rolling statistics
type MarketStats struct {
	MarketID  shared.UUID      `json:"market_id"`
	Timestamp shared.Timestamp `json:"timestamp"`

	// Price
	LastPrice      shared.Decimal `json:"last_price"`
	OpenPrice      shared.Decimal `json:"open_price"`
	HighPrice      shared.Decimal `json:"high_price"`
	LowPrice       shared.Decimal `json:"low_price"`
	PriceChange    shared.Decimal `json:"price_change"`     // Absolute
	PriceChangePct shared.Decimal `json:"price_change_pct"` // Percentage

	// Volume
	BaseVolume  shared.Decimal `json:"base_volume"`  // In base asset
	QuoteVolume shared.Decimal `json:"quote_volume"` // In quote asset
	TradeCount  int64          `json:"trade_count"`

	// Order book
	BidPrice  shared.Decimal `json:"bid_price"`
	BidSize   shared.Decimal `json:"bid_size"`
	AskPrice  shared.Decimal `json:"ask_price"`
	AskSize   shared.Decimal `json:"ask_size"`
	Spread    shared.Decimal `json:"spread"`     // Ask - Bid
	SpreadBPS int            `json:"spread_bps"` // Spread in basis points

	// Funding (for derivatives)
	FundingRate     shared.Decimal `json:"funding_rate,omitempty"`
	NextFundingTime *time.Time     `json:"next_funding_time,omitempty"`
}

// Symbol returns the market symbol (e.g., "BTC/USDT")
func (m *Market) Symbol() string {
	base := "UNKNOWN"
	quote := "UNKNOWN"
	if m.BaseAsset != nil {
		base = m.BaseAsset.Symbol
	}
	if m.QuoteAsset != nil {
		quote = m.QuoteAsset.Symbol
	}
	return base + "/" + quote
}

// IsActive checks if market is available for trading
func (m *Market) IsActive() bool {
	return m.Status == MarketStatusActive
}

// RoundPrice rounds price to the nearest multiple of the tick size. A tick
// size that is not positive means no tick, and the price is unchanged.
func (m *Market) RoundPrice(price shared.Decimal) shared.Decimal {
	if !m.TickSize.IsPositive() {
		return price
	}
	d := price.Decimal()
	tick := m.TickSize.Decimal()
	// Round to nearest tick
	ratio := d.Div(tick)
	rounded := ratio.Round(0).Mul(tick)
	return shared.Decimal(rounded)
}

// RoundQuantity rounds quantity to the nearest multiple of the lot size. A
// lot size that is not positive means no lot, and the quantity is unchanged.
func (m *Market) RoundQuantity(qty shared.Decimal) shared.Decimal {
	if !m.LotSize.IsPositive() {
		return qty
	}
	d := qty.Decimal()
	lot := m.LotSize.Decimal()
	ratio := d.Div(lot)
	rounded := ratio.Round(0).Mul(lot)
	return shared.Decimal(rounded)
}

// ValidateOrder checks if an order meets the market's requirements. A
// maximum order size of 0, the column's default, means no maximum. price is
// the limit price of a limit or stop-limit order; for a market or stop
// order, the caller passes the price it expects to fill at, such as the
// best opposite price, so that the minimum notional is checked too, or 0 to
// skip that check.
func (m *Market) ValidateOrder(side shared.OrderSide, orderType shared.OrderType, price, qty shared.Decimal) error {
	if !m.IsActive() {
		return ErrMarketInactive
	}

	if !qty.IsPositive() {
		return ErrInvalidQuantity
	}

	if qty.LessThan(m.MinOrderSize) {
		return ErrOrderSizeTooSmall
	}

	if m.MaxOrderSize.IsPositive() && qty.GreaterThan(m.MaxOrderSize) {
		return ErrOrderSizeTooLarge
	}

	if orderType == shared.OrderTypeLimit || orderType == shared.OrderTypeStopLimit {
		if price.IsZero() || price.IsNegative() {
			return ErrInvalidPrice
		}
		// Check price precision
		rounded := m.RoundPrice(price)
		if !rounded.Equal(price) {
			return ErrPricePrecision
		}
	}

	// The notional of a limit order uses its price; a market order uses the
	// expected price, when the caller knows it.
	if price.IsNegative() {
		return ErrInvalidPrice
	}
	if price.IsPositive() && price.Mul(qty).LessThan(m.MinNotional) {
		return ErrNotionalTooSmall
	}

	// Check quantity precision
	roundedQty := m.RoundQuantity(qty)
	if !roundedQty.Equal(qty) {
		return ErrQuantityPrecision
	}

	return nil
}

// Market errors
var (
	ErrMarketInactive    = NewMarketError("market is not active")
	ErrInvalidQuantity   = NewMarketError("order quantity must be positive")
	ErrOrderSizeTooSmall = NewMarketError("order size below minimum")
	ErrOrderSizeTooLarge = NewMarketError("order size exceeds maximum")
	ErrInvalidPrice      = NewMarketError("invalid price")
	ErrPricePrecision    = NewMarketError("price precision exceeds tick size")
	ErrQuantityPrecision = NewMarketError("quantity precision exceeds lot size")
	ErrNotionalTooSmall  = NewMarketError("order notional below minimum")
)

type MarketError struct {
	msg string
}

func NewMarketError(msg string) *MarketError {
	return &MarketError{msg: msg}
}

func (e *MarketError) Error() string {
	return e.msg
}
