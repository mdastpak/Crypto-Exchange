package market

import (
	"testing"
	"time"

	"encrypted-db/internal/domain/asset"
	"encrypted-db/internal/domain/shared"

	"github.com/stretchr/testify/assert"
)

func TestMarket_IsActive(t *testing.T) {
	activeMarket := &Market{Status: MarketStatusActive}
	inactiveMarket := &Market{Status: MarketStatusInactive}
	maintenanceMarket := &Market{Status: MarketStatusMaintenance}

	assert.True(t, activeMarket.IsActive())
	assert.False(t, inactiveMarket.IsActive())
	assert.False(t, maintenanceMarket.IsActive())
}

func TestMarket_Symbol(t *testing.T) {
	market := &Market{
		BaseAssetID:  shared.NewUUID(),
		QuoteAssetID: shared.NewUUID(),
		BaseAsset:    &asset.Asset{Symbol: "BTC"},
		QuoteAsset:   &asset.Asset{Symbol: "USDT"},
	}
	assert.Equal(t, "BTC/USDT", market.Symbol())

	// Without assets loaded
	market2 := &Market{
		BaseAssetID:  shared.NewUUID(),
		QuoteAssetID: shared.NewUUID(),
	}
	assert.Equal(t, "UNKNOWN/UNKNOWN", market2.Symbol())
}

func TestMarket_RoundPrice(t *testing.T) {
	market := &Market{
		TickSize: shared.MustNewDecimalFromString("0.01"),
	}

	// Exact tick size - Decimal.String() may not preserve trailing zeros
	price := shared.MustNewDecimalFromString("100.00")
	rounded := market.RoundPrice(price)
	// Just verify the value is correct (100.00 == 100)
	assert.Equal(t, shared.MustNewDecimalFromString("100").String(), rounded.String())

	// Needs rounding up
	price = shared.MustNewDecimalFromString("100.005")
	rounded = market.RoundPrice(price)
	assert.Equal(t, shared.MustNewDecimalFromString("100.01").String(), rounded.String())

	// Needs rounding down
	price = shared.MustNewDecimalFromString("100.004")
	rounded = market.RoundPrice(price)
	assert.Equal(t, shared.MustNewDecimalFromString("100").String(), rounded.String())

	// Zero tick size (no rounding)
	market2 := &Market{TickSize: shared.Decimal{}}
	price = shared.MustNewDecimalFromString("100.123456")
	rounded = market2.RoundPrice(price)
	assert.Equal(t, shared.MustNewDecimalFromString("100.123456").String(), rounded.String())
}

func TestMarket_RoundQuantity(t *testing.T) {
	market := &Market{
		LotSize: shared.MustNewDecimalFromString("0.001"),
	}

	// Exact lot size
	qty := shared.MustNewDecimalFromString("1.000")
	rounded := market.RoundQuantity(qty)
	assert.Equal(t, shared.MustNewDecimalFromString("1").String(), rounded.String())

	// Needs rounding
	qty = shared.MustNewDecimalFromString("1.0005")
	rounded = market.RoundQuantity(qty)
	assert.Equal(t, shared.MustNewDecimalFromString("1.001").String(), rounded.String())

	qty = shared.MustNewDecimalFromString("1.0004")
	rounded = market.RoundQuantity(qty)
	assert.Equal(t, shared.MustNewDecimalFromString("1").String(), rounded.String())
}

func TestMarket_ValidateOrder(t *testing.T) {
	market := &Market{
		Status:       MarketStatusActive,
		MinOrderSize: shared.MustNewDecimalFromString("0.001"),
		MaxOrderSize: shared.MustNewDecimalFromString("1000"),
		MinNotional:  shared.MustNewDecimalFromString("10"),
		TickSize:     shared.MustNewDecimalFromString("0.01"),
		LotSize:      shared.MustNewDecimalFromString("0.001"),
		MakerFeeBPS:  10,
		TakerFeeBPS:  20,
	}

	tests := []struct {
		name      string
		side      shared.OrderSide
		orderType shared.OrderType
		price     shared.Decimal
		qty       shared.Decimal
		wantError bool
		errorType error
	}{
		{
			name:      "valid limit buy",
			side:      shared.OrderSideBuy,
			orderType: shared.OrderTypeLimit,
			price:     shared.MustNewDecimalFromString("50000.00"),
			qty:       shared.MustNewDecimalFromString("0.1"),
			wantError: false,
		},
		{
			name:      "valid limit sell",
			side:      shared.OrderSideSell,
			orderType: shared.OrderTypeLimit,
			price:     shared.MustNewDecimalFromString("50000.00"),
			qty:       shared.MustNewDecimalFromString("0.1"),
			wantError: false,
		},
		{
			name:      "valid market order",
			side:      shared.OrderSideBuy,
			orderType: shared.OrderTypeMarket,
			price:     shared.Decimal{}, // Zero price for market
			qty:       shared.MustNewDecimalFromString("0.1"),
			wantError: false,
		},
		{
			name:      "inactive market",
			side:      shared.OrderSideBuy,
			orderType: shared.OrderTypeLimit,
			price:     shared.MustNewDecimalFromString("50000.00"),
			qty:       shared.MustNewDecimalFromString("0.1"),
			wantError: true,
			errorType: ErrMarketInactive,
		},
		{
			name:      "quantity too small",
			side:      shared.OrderSideBuy,
			orderType: shared.OrderTypeLimit,
			price:     shared.MustNewDecimalFromString("50000.00"),
			qty:       shared.MustNewDecimalFromString("0.0001"),
			wantError: true,
			errorType: ErrOrderSizeTooSmall,
		},
		{
			name:      "quantity too large",
			side:      shared.OrderSideBuy,
			orderType: shared.OrderTypeLimit,
			price:     shared.MustNewDecimalFromString("50000.00"),
			qty:       shared.MustNewDecimalFromString("2000"),
			wantError: true,
			errorType: ErrOrderSizeTooLarge,
		},
		{
			name:      "zero price for limit",
			side:      shared.OrderSideBuy,
			orderType: shared.OrderTypeLimit,
			price:     shared.Decimal{},
			qty:       shared.MustNewDecimalFromString("0.1"),
			wantError: true,
			errorType: ErrInvalidPrice,
		},
		{
			name:      "negative price",
			side:      shared.OrderSideBuy,
			orderType: shared.OrderTypeLimit,
			price:     shared.MustNewDecimalFromString("-100"),
			qty:       shared.MustNewDecimalFromString("0.1"),
			wantError: true,
			errorType: ErrInvalidPrice,
		},
		{
			name:      "price precision error",
			side:      shared.OrderSideBuy,
			orderType: shared.OrderTypeLimit,
			price:     shared.MustNewDecimalFromString("50000.005"), // Not multiple of 0.01
			qty:       shared.MustNewDecimalFromString("0.1"),
			wantError: true,
			errorType: ErrPricePrecision,
		},
		{
			name:      "notional too small",
			side:      shared.OrderSideBuy,
			orderType: shared.OrderTypeLimit,
			price:     shared.MustNewDecimalFromString("10"),
			qty:       shared.MustNewDecimalFromString("0.1"), // 1.0 notional < 10 min
			wantError: true,
			errorType: ErrNotionalTooSmall,
		},
		{
			name:      "quantity precision error",
			side:      shared.OrderSideBuy,
			orderType: shared.OrderTypeLimit,
			price:     shared.MustNewDecimalFromString("50000.00"),
			qty:       shared.MustNewDecimalFromString("1.0005"), // Above min, but not multiple of 0.001
			wantError: true,
			errorType: ErrQuantityPrecision,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := *market
			if tt.name == "inactive market" {
				m.Status = MarketStatusInactive
			}
			err := m.ValidateOrder(tt.side, tt.orderType, tt.price, tt.qty)
			if tt.wantError {
				assert.Error(t, err)
				if tt.errorType != nil {
					assert.Equal(t, tt.errorType.Error(), err.Error())
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestMarketStatus_String(t *testing.T) {
	assert.Equal(t, "ACTIVE", MarketStatusActive.String())
	assert.Equal(t, "INACTIVE", MarketStatusInactive.String())
	assert.Equal(t, "MAINTENANCE", MarketStatusMaintenance.String())
	assert.Equal(t, "SUSPENDED", MarketStatusSuspended.String())
	assert.Equal(t, "DELISTED", MarketStatusDelisted.String())
}

func TestMarketStats_Calculations(t *testing.T) {
	stats := &MarketStats{
		LastPrice:   shared.MustNewDecimalFromString("50000"),
		OpenPrice:   shared.MustNewDecimalFromString("49000"),
		HighPrice:   shared.MustNewDecimalFromString("51000"),
		LowPrice:    shared.MustNewDecimalFromString("48000"),
		BaseVolume:  shared.MustNewDecimalFromString("100"),
		QuoteVolume: shared.MustNewDecimalFromString("5000000"),
		TradeCount:  1000,
		BidPrice:    shared.MustNewDecimalFromString("49999"),
		BidSize:     shared.MustNewDecimalFromString("10"),
		AskPrice:    shared.MustNewDecimalFromString("50001"),
		AskSize:     shared.MustNewDecimalFromString("15"),
	}

	// Spread = Ask - Bid
	spread := stats.AskPrice.Sub(stats.BidPrice)
	assert.Equal(t, "2", spread.String())

	// Spread BPS = (Spread / Mid) * 10000
	// Mid = (50001 + 49999) / 2 = 50000
	// Spread = 2
	// BPS = (2 / 50000) * 10000 = 0.4
	mid := stats.AskPrice.Add(stats.BidPrice).Div(shared.MustNewDecimalFromString("2"))
	spreadBPS := spread.Div(mid).Mul(shared.MustNewDecimalFromString("10000"))
	assert.InDelta(t, 0.4, spreadBPS.InexactFloat64(), 0.1)
}

func TestMarketStats_PriceChange(t *testing.T) {
	stats := &MarketStats{
		LastPrice:      shared.MustNewDecimalFromString("55000"),
		OpenPrice:      shared.MustNewDecimalFromString("50000"),
		PriceChange:    shared.MustNewDecimalFromString("5000"),
		PriceChangePct: shared.MustNewDecimalFromString("10"),
	}

	// Verify price change calculation
	expectedChange := stats.LastPrice.Sub(stats.OpenPrice)
	assert.Equal(t, stats.PriceChange.String(), expectedChange.String())

	// Verify percentage
	expectedPct := stats.PriceChange.Div(stats.OpenPrice).Mul(shared.MustNewDecimalFromString("100"))
	assert.Equal(t, stats.PriceChangePct.String(), expectedPct.String())
}

func TestMarket_ValidateOrder_StopLimit(t *testing.T) {
	market := &Market{
		Status:       MarketStatusActive,
		MinOrderSize: shared.MustNewDecimalFromString("0.001"),
		MaxOrderSize: shared.MustNewDecimalFromString("1000"),
		MinNotional:  shared.MustNewDecimalFromString("10"),
		TickSize:     shared.MustNewDecimalFromString("0.01"),
		LotSize:      shared.MustNewDecimalFromString("0.001"),
	}

	// Stop limit order needs both stop price and limit price
	// For this test, we treat it like a limit order for validation
	err := market.ValidateOrder(shared.OrderSideBuy, shared.OrderTypeStopLimit,
		shared.MustNewDecimalFromString("50000.00"),
		shared.MustNewDecimalFromString("0.1"))
	assert.NoError(t, err)

	// Invalid stop price (zero)
	err = market.ValidateOrder(shared.OrderSideBuy, shared.OrderTypeStopLimit,
		shared.Decimal{},
		shared.MustNewDecimalFromString("0.1"))
	assert.Error(t, err)
	assert.Equal(t, ErrInvalidPrice.Error(), err.Error())
}

func TestMarketError_Error(t *testing.T) {
	err := NewMarketError("test error message")
	assert.Equal(t, "test error message", err.Error())
}

func TestMarketStats_ZeroValues(t *testing.T) {
	stats := &MarketStats{
		Timestamp: shared.Timestamp(time.Now()),
	}
	// All zero values should be handled gracefully
	assert.True(t, stats.LastPrice.IsZero())
	assert.True(t, stats.BaseVolume.IsZero())
	assert.Equal(t, int64(0), stats.TradeCount)
}
