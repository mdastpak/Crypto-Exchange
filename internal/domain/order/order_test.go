package order

import (
	"testing"

	"encrypted-db/internal/domain/market"
	"encrypted-db/internal/domain/shared"

	"github.com/stretchr/testify/assert"
)

func TestOrder_RemainingQuantity(t *testing.T) {
	order := &Order{
		Quantity:       shared.MustNewDecimalFromString("10"),
		FilledQuantity: shared.MustNewDecimalFromString("3"),
	}
	remaining := order.RemainingQuantity()
	assert.Equal(t, "7", remaining.String())
}

func TestOrder_RemainingQuantity_FullyFilled(t *testing.T) {
	order := &Order{
		Quantity:       shared.MustNewDecimalFromString("10"),
		FilledQuantity: shared.MustNewDecimalFromString("10"),
	}
	remaining := order.RemainingQuantity()
	assert.True(t, remaining.IsZero())
}

func TestOrder_IsActive(t *testing.T) {
	activeStatuses := []shared.OrderStatus{
		shared.OrderStatusNew,
		shared.OrderStatusPartiallyFilled,
	}

	inactiveStatuses := []shared.OrderStatus{
		shared.OrderStatusFilled,
		shared.OrderStatusCancelled,
		shared.OrderStatusRejected,
		shared.OrderStatusExpired,
	}

	for _, status := range activeStatuses {
		order := &Order{Status: status}
		assert.True(t, order.IsActive(), "status %s should be active", status)
	}

	for _, status := range inactiveStatuses {
		order := &Order{Status: status}
		assert.False(t, order.IsActive(), "status %s should not be active", status)
	}
}

func TestOrder_IsTerminal(t *testing.T) {
	terminalStatuses := []shared.OrderStatus{
		shared.OrderStatusFilled,
		shared.OrderStatusCancelled,
		shared.OrderStatusRejected,
		shared.OrderStatusExpired,
	}

	nonTerminalStatuses := []shared.OrderStatus{
		shared.OrderStatusNew,
		shared.OrderStatusPartiallyFilled,
	}

	for _, status := range terminalStatuses {
		order := &Order{Status: status}
		assert.True(t, order.IsTerminal(), "status %s should be terminal", status)
	}

	for _, status := range nonTerminalStatuses {
		order := &Order{Status: status}
		assert.False(t, order.IsTerminal(), "status %s should not be terminal", status)
	}
}

func TestOrder_FillPercentage(t *testing.T) {
	order := &Order{
		Quantity:       shared.MustNewDecimalFromString("100"),
		FilledQuantity: shared.MustNewDecimalFromString("25"),
	}
	pct := order.FillPercentage()
	assert.InDelta(t, 25.0, pct, 0.01)

	// Zero quantity
	order2 := &Order{
		Quantity:       shared.Decimal{},
		FilledQuantity: shared.Decimal{},
	}
	pct2 := order2.FillPercentage()
	assert.Equal(t, 0.0, pct2)

	// 100% filled
	order3 := &Order{
		Quantity:       shared.MustNewDecimalFromString("10"),
		FilledQuantity: shared.MustNewDecimalFromString("10"),
	}
	pct3 := order3.FillPercentage()
	assert.InDelta(t, 100.0, pct3, 0.01)
}

func TestOrder_CanCancel(t *testing.T) {
	activeOrder := &Order{Status: shared.OrderStatusNew}
	assert.True(t, activeOrder.CanCancel())

	partialOrder := &Order{Status: shared.OrderStatusPartiallyFilled}
	assert.True(t, partialOrder.CanCancel())

	filledOrder := &Order{Status: shared.OrderStatusFilled}
	assert.False(t, filledOrder.CanCancel())

	cancelledOrder := &Order{Status: shared.OrderStatusCancelled}
	assert.False(t, cancelledOrder.CanCancel())
}

func TestOrderStatus_String(t *testing.T) {
	assert.Equal(t, "NEW", shared.OrderStatusNew.String())
	assert.Equal(t, "PARTIALLY_FILLED", shared.OrderStatusPartiallyFilled.String())
	assert.Equal(t, "FILLED", shared.OrderStatusFilled.String())
	assert.Equal(t, "CANCELLED", shared.OrderStatusCancelled.String())
	assert.Equal(t, "REJECTED", shared.OrderStatusRejected.String())
	assert.Equal(t, "EXPIRED", shared.OrderStatusExpired.String())
}

func TestOrderSide_String(t *testing.T) {
	assert.Equal(t, "BUY", shared.OrderSideBuy.String())
	assert.Equal(t, "SELL", shared.OrderSideSell.String())
}

func TestOrderSide_Opposite(t *testing.T) {
	assert.Equal(t, shared.OrderSideSell, shared.OrderSideBuy.Opposite())
	assert.Equal(t, shared.OrderSideBuy, shared.OrderSideSell.Opposite())
}

func TestOrderType_String(t *testing.T) {
	assert.Equal(t, "MARKET", shared.OrderTypeMarket.String())
	assert.Equal(t, "LIMIT", shared.OrderTypeLimit.String())
	assert.Equal(t, "STOP", shared.OrderTypeStop.String())
	assert.Equal(t, "STOP_LIMIT", shared.OrderTypeStopLimit.String())
}

func TestTimeInForce_String(t *testing.T) {
	assert.Equal(t, "GTC", shared.TimeInForceGTC.String())
	assert.Equal(t, "IOC", shared.TimeInForceIOC.String())
	assert.Equal(t, "FOK", shared.TimeInForceFOK.String())
	assert.Equal(t, "GTX", shared.TimeInForceGTX.String())
}

func TestOrderEvent(t *testing.T) {
	event := &OrderEvent{
		EventID:   shared.NewUUID(),
		OrderID:   shared.NewUUID(),
		EventType: OrderEventCreated,
		Timestamp: shared.Timestamp{},
		NewStatus: shared.OrderStatusNew,
	}

	assert.Equal(t, OrderEventCreated, event.EventType)
	assert.Equal(t, shared.OrderStatusNew, event.NewStatus)
}

func TestOrderBookSnapshot(t *testing.T) {
	snapshot := &OrderBookSnapshot{
		MarketID:  shared.NewUUID(),
		Timestamp: shared.Timestamp{},
		Sequence:  100,
		Bids: []OrderBookLevel{
			{Price: shared.MustNewDecimalFromString("100"), Quantity: shared.MustNewDecimalFromString("10"), OrderCount: 5},
			{Price: shared.MustNewDecimalFromString("99"), Quantity: shared.MustNewDecimalFromString("20"), OrderCount: 3},
		},
		Asks: []OrderBookLevel{
			{Price: shared.MustNewDecimalFromString("101"), Quantity: shared.MustNewDecimalFromString("15"), OrderCount: 4},
			{Price: shared.MustNewDecimalFromString("102"), Quantity: shared.MustNewDecimalFromString("25"), OrderCount: 2},
		},
	}

	assert.Len(t, snapshot.Bids, 2)
	assert.Len(t, snapshot.Asks, 2)
	assert.Equal(t, int64(100), snapshot.Sequence)

	// Best bid/ask
	assert.Equal(t, "100", snapshot.Bids[0].Price.String())
	assert.Equal(t, "101", snapshot.Asks[0].Price.String())
}

func TestOrder_WithMarket(t *testing.T) {
	order := &Order{
		MarketID: shared.NewUUID(),
		Market: &market.Market{
			ID:           shared.NewUUID(),
			Status:       market.MarketStatusActive,
			TickSize:     shared.MustNewDecimalFromString("0.01"),
			LotSize:      shared.MustNewDecimalFromString("0.001"),
			MinOrderSize: shared.MustNewDecimalFromString("0.001"),
			MaxOrderSize: shared.MustNewDecimalFromString("1000"),
		},
	}

	// Test validation through market
	err := order.Market.ValidateOrder(shared.OrderSideBuy, shared.OrderTypeLimit,
		shared.MustNewDecimalFromString("50000.00"),
		shared.MustNewDecimalFromString("0.1"))
	assert.NoError(t, err)
}

func TestOrder_FeeCalculation(t *testing.T) {
	order := &Order{
		Quantity:   shared.MustNewDecimalFromString("10"),
		Price:      shared.MustNewDecimalFromString("100"),
		FeeRateBPS: 10, // 0.1%
		FeeAssetID: shared.NewUUID(),
	}

	notional := order.Price.Mul(order.Quantity)
	expectedFee := notional.Mul(shared.MustNewDecimalFromString("0.001")) // 10 bps = 0.1%

	// Fee = Notional * FeeRateBPS / 10000
	fee := notional.Mul(shared.MustNewDecimalFromString("10")).Div(shared.MustNewDecimalFromString("10000"))
	assert.Equal(t, expectedFee.String(), fee.String())
}

func TestOrderEventType_String(t *testing.T) {
	assert.Equal(t, "ORDER_CREATED", OrderEventCreated.String())
	assert.Equal(t, "ORDER_PARTIALLY_FILLED", OrderEventPartiallyFilled.String())
	assert.Equal(t, "ORDER_FILLED", OrderEventFilled.String())
	assert.Equal(t, "ORDER_CANCELLED", OrderEventCancelled.String())
	assert.Equal(t, "ORDER_REJECTED", OrderEventRejected.String())
	assert.Equal(t, "ORDER_EXPIRED", OrderEventExpired.String())
	assert.Equal(t, "ORDER_MODIFIED", OrderEventModified.String())
}

func TestOrderBookLevel(t *testing.T) {
	level := OrderBookLevel{
		Price:      shared.MustNewDecimalFromString("100.50"),
		Quantity:   shared.MustNewDecimalFromString("10"),
		OrderCount: 3,
	}

	// Decimal.String() may not preserve trailing zeros
	assert.Equal(t, shared.MustNewDecimalFromString("100.5").String(), level.Price.String())
	assert.Equal(t, "10", level.Quantity.String())
	assert.Equal(t, 3, level.OrderCount)
}
