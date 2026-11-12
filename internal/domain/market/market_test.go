package market

import (
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		BaseCurrencyHK:  shared.NewUUID(),
		QuoteCurrencyHK: shared.NewUUID(),
		BaseSymbol:      "BTC",
		QuoteSymbol:     "USDT",
	}
	assert.Equal(t, "BTC/USDT", market.Symbol())

	// Without the symbols read
	market2 := &Market{
		BaseCurrencyHK:  shared.NewUUID(),
		QuoteCurrencyHK: shared.NewUUID(),
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
	mid, err := stats.AskPrice.Add(stats.BidPrice).Div(shared.MustNewDecimalFromString("2"))
	require.NoError(t, err)
	share, err := spread.Div(mid)
	require.NoError(t, err)
	spreadBPS := share.Mul(shared.MustNewDecimalFromString("10000"))
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
	ratio, err := stats.PriceChange.Div(stats.OpenPrice)
	require.NoError(t, err)
	expectedPct := ratio.Mul(shared.MustNewDecimalFromString("100"))
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

func dec(s string) shared.Decimal { return shared.MustNewDecimalFromString(s) }

// TestMarket_ValidateOrder_EdgeValues covers zero, negative, and boundary
// values, with the column defaults: a maximum order size of 0 means no
// maximum.
func TestMarket_ValidateOrder_EdgeValues(t *testing.T) {
	base := Market{
		Status:       MarketStatusActive,
		MinOrderSize: dec("0.001"),
		MaxOrderSize: dec("100"),
		MinNotional:  dec("10"),
		TickSize:     dec("0.01"),
		LotSize:      dec("0.001"),
	}
	for name, tc := range map[string]struct {
		change    func(m *Market)
		orderType shared.OrderType
		price     string
		qty       string
		want      error
	}{
		"limit within every limit":      {nil, shared.OrderTypeLimit, "50000", "1", nil},
		"quantity at the minimum":       {nil, shared.OrderTypeLimit, "50000", "0.001", nil},
		"quantity at the maximum":       {nil, shared.OrderTypeLimit, "50000", "100", nil},
		"quantity over the maximum":     {nil, shared.OrderTypeLimit, "50000", "100.001", ErrOrderSizeTooLarge},
		"no maximum (0)":                {func(m *Market) { m.MaxOrderSize = dec("0") }, shared.OrderTypeLimit, "50000", "1000000", nil},
		"zero quantity, no minimum":     {func(m *Market) { m.MinOrderSize = dec("0") }, shared.OrderTypeLimit, "50000", "0", ErrInvalidQuantity},
		"negative quantity, no minimum": {func(m *Market) { m.MinOrderSize = dec("0") }, shared.OrderTypeMarket, "0", "-1", ErrInvalidQuantity},
		"notional at the minimum":       {nil, shared.OrderTypeLimit, "10", "1", nil},
		"notional under the minimum":    {nil, shared.OrderTypeLimit, "9.99", "1", ErrNotionalTooSmall},
		"market order, expected price":  {nil, shared.OrderTypeMarket, "9.99", "1", ErrNotionalTooSmall},
		"market order, no price known":  {nil, shared.OrderTypeMarket, "0", "1", nil},
		"market order, negative price":  {nil, shared.OrderTypeMarket, "-1", "1", ErrInvalidPrice},
		"limit order without a price":   {nil, shared.OrderTypeLimit, "0", "1", ErrInvalidPrice},
		"limit off the tick":            {nil, shared.OrderTypeLimit, "50000.005", "1", ErrPricePrecision},
		"quantity off the lot":          {nil, shared.OrderTypeLimit, "50000", "1.0005", ErrQuantityPrecision},
		"no tick or lot size (0)":       {func(m *Market) { m.TickSize, m.LotSize = dec("0"), dec("0") }, shared.OrderTypeLimit, "50000.123456", "1.23456", nil},
		"negative tick and lot size":    {func(m *Market) { m.TickSize, m.LotSize = dec("-1"), dec("-1") }, shared.OrderTypeLimit, "50000.123456", "1.23456", nil},
		"inactive market":               {func(m *Market) { m.Status = MarketStatusSuspended }, shared.OrderTypeLimit, "50000", "1", ErrMarketInactive},
	} {
		t.Run(name, func(t *testing.T) {
			m := base
			if tc.change != nil {
				tc.change(&m)
			}
			var err error
			assert.NotPanics(t, func() {
				err = m.ValidateOrder(shared.OrderSideBuy, tc.orderType, dec(tc.price), dec(tc.qty))
			})
			assert.Equal(t, tc.want, err)
		})
	}
}
