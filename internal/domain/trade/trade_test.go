package trade

import (
	"testing"

	"encrypted-db/internal/domain/market"
	"encrypted-db/internal/domain/order"
	"encrypted-db/internal/domain/shared"

	"github.com/stretchr/testify/assert"
)

func TestTrade_Notional(t *testing.T) {
	trade := &Trade{
		Price:    shared.MustNewDecimalFromString("50000"),
		Quantity: shared.MustNewDecimalFromString("0.1"),
	}
	notional := trade.Notional()
	assert.Equal(t, "5000", notional.String())
}

func TestTrade_TotalFees(t *testing.T) {
	trade := &Trade{
		MakerFee: shared.MustNewDecimalFromString("2.5"),
		TakerFee: shared.MustNewDecimalFromString("5.0"),
	}
	totalFees := trade.TotalFees()
	assert.Equal(t, "7.5", totalFees.String())
}

func TestTrade_IsMaker(t *testing.T) {
	makerOrderID := shared.NewUUID()
	takerOrderID := shared.NewUUID()

	trade := &Trade{
		MakerOrderID: makerOrderID,
		TakerOrderID: takerOrderID,
	}

	assert.True(t, trade.IsMaker(makerOrderID))
	assert.False(t, trade.IsMaker(takerOrderID))
	assert.False(t, trade.IsMaker(shared.NewUUID()))
}

func TestTrade_IsTaker(t *testing.T) {
	makerOrderID := shared.NewUUID()
	takerOrderID := shared.NewUUID()

	trade := &Trade{
		MakerOrderID: makerOrderID,
		TakerOrderID: takerOrderID,
	}

	assert.True(t, trade.IsTaker(takerOrderID))
	assert.False(t, trade.IsTaker(makerOrderID))
	assert.False(t, trade.IsTaker(shared.NewUUID()))
}

func TestTrade_GetFeeForOrder(t *testing.T) {
	makerOrderID := shared.NewUUID()
	takerOrderID := shared.NewUUID()
	makerFeeAssetID := shared.NewUUID()
	takerFeeAssetID := shared.NewUUID()

	trade := &Trade{
		MakerOrderID:    makerOrderID,
		TakerOrderID:    takerOrderID,
		MakerFee:        shared.MustNewDecimalFromString("2.5"),
		MakerFeeAssetID: makerFeeAssetID,
		TakerFee:        shared.MustNewDecimalFromString("5.0"),
		TakerFeeAssetID: takerFeeAssetID,
	}

	// Maker fee
	fee, assetID := trade.GetFeeForOrder(makerOrderID)
	assert.Equal(t, shared.MustNewDecimalFromString("2.5").String(), fee.String())
	assert.Equal(t, makerFeeAssetID, assetID)

	// Taker fee
	fee, assetID = trade.GetFeeForOrder(takerOrderID)
	assert.Equal(t, shared.MustNewDecimalFromString("5").String(), fee.String())
	assert.Equal(t, takerFeeAssetID, assetID)

	// Unknown order
	fee, assetID = trade.GetFeeForOrder(shared.NewUUID())
	assert.True(t, fee.IsZero())
	assert.True(t, assetID.IsZero())
}

func TestTradeSide_String(t *testing.T) {
	assert.Equal(t, "BUY", shared.TradeSideBuy.String())
	assert.Equal(t, "SELL", shared.TradeSideSell.String())
}

func TestSettlementMode_String(t *testing.T) {
	assert.Equal(t, "OFF_CHAIN", shared.SettlementModeOffChain.String())
	assert.Equal(t, "ON_CHAIN", shared.SettlementModeOnChain.String())
	assert.Equal(t, "LAYER2", shared.SettlementModeLayer2.String())
	assert.Equal(t, "BATCH", shared.SettlementModeBatch.String())
}

func TestTradeEvent(t *testing.T) {
	event := &TradeEvent{
		EventID:        shared.NewUUID(),
		TradeID:        shared.NewUUID(),
		EventType:      TradeEventExecuted,
		Timestamp:      shared.Timestamp{},
		MarketID:       shared.NewUUID(),
		Side:           shared.TradeSideBuy,
		Price:          shared.MustNewDecimalFromString("50000"),
		Quantity:       shared.MustNewDecimalFromString("0.1"),
		MakerOrderID:   shared.NewUUID(),
		TakerOrderID:   shared.NewUUID(),
		MakerUserID:    shared.NewUUID(),
		TakerUserID:    shared.NewUUID(),
		MakerFee:       shared.MustNewDecimalFromString("2.5"),
		TakerFee:       shared.MustNewDecimalFromString("5.0"),
		SettlementMode: shared.SettlementModeOffChain,
	}

	assert.Equal(t, TradeEventExecuted, event.EventType)
	assert.Equal(t, shared.TradeSideBuy, event.Side)
	assert.Equal(t, "50000", event.Price.String())
}

func TestTradeEventType_String(t *testing.T) {
	assert.Equal(t, "TRADE_EXECUTED", TradeEventExecuted.String())
	assert.Equal(t, "TRADE_SETTLED", TradeEventSettled.String())
}

func TestTradeAggregate(t *testing.T) {
	agg := &TradeAggregate{
		MarketID:    shared.NewUUID(),
		Interval:    "1h",
		StartTime:   shared.Timestamp{},
		EndTime:     shared.Timestamp{},
		OpenPrice:   shared.MustNewDecimalFromString("50000"),
		HighPrice:   shared.MustNewDecimalFromString("51000"),
		LowPrice:    shared.MustNewDecimalFromString("49000"),
		ClosePrice:  shared.MustNewDecimalFromString("50500"),
		BaseVolume:  shared.MustNewDecimalFromString("100"),
		QuoteVolume: shared.MustNewDecimalFromString("5050000"),
		TradeCount:  1000,
		VWAP:        shared.MustNewDecimalFromString("50500"),
		BuyVolume:   shared.MustNewDecimalFromString("60"),
		SellVolume:  shared.MustNewDecimalFromString("40"),
		BuyCount:    600,
		SellCount:   400,
	}

	// VWAP verification
	expectedVWAP := agg.QuoteVolume.Div(agg.BaseVolume)
	assert.Equal(t, expectedVWAP.String(), agg.VWAP.String())

	// Volume consistency
	totalVolume := agg.BuyVolume.Add(agg.SellVolume)
	assert.Equal(t, agg.BaseVolume.String(), totalVolume.String())

	// Count consistency
	totalCount := agg.BuyCount + agg.SellCount
	assert.Equal(t, agg.TradeCount, totalCount)
}

func TestTrade_WithMarketAndOrders(t *testing.T) {
	makerOrderID := shared.NewUUID()
	takerOrderID := shared.NewUUID()

	trade := &Trade{
		MarketID: shared.NewUUID(),
		Market: &market.Market{
			ID:           shared.NewUUID(),
			BaseAssetID:  shared.NewUUID(),
			QuoteAssetID: shared.NewUUID(),
		},
		MakerOrderID: makerOrderID,
		MakerOrder: &order.Order{
			ID:     makerOrderID,
			UserID: shared.NewUUID(),
		},
		TakerOrderID: takerOrderID,
		TakerOrder: &order.Order{
			ID:     takerOrderID,
			UserID: shared.NewUUID(),
		},
	}

	assert.NotNil(t, trade.Market)
	assert.NotNil(t, trade.MakerOrder)
	assert.NotNil(t, trade.TakerOrder)
	assert.Equal(t, trade.MakerOrderID, trade.MakerOrder.ID)
	assert.Equal(t, trade.TakerOrderID, trade.TakerOrder.ID)
}
