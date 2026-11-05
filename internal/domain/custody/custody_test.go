package custody

import (
	"testing"

	"encrypted-db/internal/domain/shared"

	"github.com/stretchr/testify/assert"
)

func TestDepositAddress(t *testing.T) {
	addr := &DepositAddress{
		ID:             shared.NewUUID(),
		UserID:         shared.NewUUID(),
		SubAccountID:   func() *shared.UUID { id := shared.NewUUID(); return &id }(),
		AssetID:        shared.NewUUID(),
		Address:        "0x1234567890abcdef",
		Tag:            "12345",
		DerivationPath: "m/44'/60'/0'/0/0",
		Index:          0,
		Active:         true,
	}

	assert.NotEmpty(t, addr.ID)
	assert.NotEmpty(t, addr.Address)
	assert.Equal(t, "12345", addr.Tag)
	assert.True(t, addr.Active)
}

func TestDepositAddress_Minimal(t *testing.T) {
	addr := &DepositAddress{
		ID:      shared.NewUUID(),
		UserID:  shared.NewUUID(),
		AssetID: shared.NewUUID(),
		Address: "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh",
		Active:  true,
	}

	assert.NotEmpty(t, addr.ID)
	assert.Equal(t, "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh", addr.Address)
	assert.Empty(t, addr.Tag)
	assert.Equal(t, uint32(0), addr.Index)
}

func TestDepositStatus(t *testing.T) {
	status := &DepositStatus{
		TxHash:           "0xabc123...",
		AssetID:          shared.NewUUID(),
		Address:          "0x123...",
		Amount:           shared.MustNewDecimalFromString("1.5"),
		ConfirmedAmount:  shared.MustNewDecimalFromString("1.5"),
		Confirmations:    12,
		RequiredConfirms: 12,
		Status:           DepositStatusConfirmed,
		DetectedAt:       shared.Timestamp{},
		ConfirmedAt:      func() *shared.Timestamp { t := shared.Timestamp{}; return &t }(),
		CreditedAt:       func() *shared.Timestamp { t := shared.Timestamp{}; return &t }(),
		BlockHeight:      18000000,
		BlockHash:        "0xblockhash...",
		RawData:          `{"confirmations": 12}`,
	}

	assert.Equal(t, DepositStatusConfirmed, status.Status)
	assert.Equal(t, 12, status.Confirmations)
	assert.Equal(t, "1.5", status.Amount.String())
	assert.Equal(t, uint64(18000000), status.BlockHeight)
}

func TestDepositStatus_Transitions(t *testing.T) {
	// Detected -> Confirming -> Confirmed -> Credited
	detected := &DepositStatus{
		Status:           DepositStatusDetected,
		Confirmations:    0,
		RequiredConfirms: 12,
	}
	assert.Equal(t, DepositStatusDetected, detected.Status)

	confirming := &DepositStatus{
		Status:           DepositStatusConfirming,
		Confirmations:    6,
		RequiredConfirms: 12,
	}
	assert.Equal(t, DepositStatusConfirming, confirming.Status)

	confirmed := &DepositStatus{
		Status:           DepositStatusConfirmed,
		Confirmations:    12,
		RequiredConfirms: 12,
	}
	assert.Equal(t, DepositStatusConfirmed, confirmed.Status)
	assert.True(t, confirmed.Confirmations >= confirmed.RequiredConfirms)

	credited := &DepositStatus{
		Status:           DepositStatusCredited,
		Confirmations:    12,
		RequiredConfirms: 12,
	}
	assert.Equal(t, DepositStatusCredited, credited.Status)
}

func TestDepositStatusType_String(t *testing.T) {
	assert.Equal(t, "DETECTED", DepositStatusDetected.String())
	assert.Equal(t, "CONFIRMING", DepositStatusConfirming.String())
	assert.Equal(t, "CONFIRMED", DepositStatusConfirmed.String())
	assert.Equal(t, "CREDITED", DepositStatusCredited.String())
	assert.Equal(t, "FAILED", DepositStatusFailed.String())
}

func TestWithdrawalRequest(t *testing.T) {
	req := &WithdrawalRequest{
		ID:             shared.NewUUID(),
		UserID:         shared.NewUUID(),
		SubAccountID:   func() *shared.UUID { id := shared.NewUUID(); return &id }(),
		AssetID:        shared.NewUUID(),
		Amount:         shared.MustNewDecimalFromString("10"),
		Address:        "0xabcdef...",
		Tag:            "memo123",
		FeeLevel:       FeeLevelNormal,
		SubtractFee:    false,
		Priority:       true,
		IdempotencyKey: "idem-key-123",
		CreatedAt:      shared.Timestamp{},
	}

	assert.Equal(t, FeeLevelNormal, req.FeeLevel)
	assert.False(t, req.SubtractFee)
	assert.True(t, req.Priority)
	assert.Equal(t, "idem-key-123", req.IdempotencyKey)
}

func TestWithdrawalRequest_CustomFee(t *testing.T) {
	customFee := shared.MustNewDecimalFromString("0.001")
	req := &WithdrawalRequest{
		FeeLevel:    FeeLevelCustom,
		CustomFee:   &customFee,
		SubtractFee: true,
	}

	assert.Equal(t, FeeLevelCustom, req.FeeLevel)
	assert.NotNil(t, req.CustomFee)
	assert.Equal(t, "0.001", req.CustomFee.String())
	assert.True(t, req.SubtractFee)
}

func TestFeeLevel_String(t *testing.T) {
	assert.Equal(t, "SLOW", FeeLevelSlow.String())
	assert.Equal(t, "NORMAL", FeeLevelNormal.String())
	assert.Equal(t, "FAST", FeeLevelFast.String())
	assert.Equal(t, "CUSTOM", FeeLevelCustom.String())
}

func TestWithdrawalResult(t *testing.T) {
	txHash := "0xtxhash..."
	result := &WithdrawalResult{
		WithdrawalID: shared.NewUUID(),
		TxHash:       &txHash,
		Status:       WithdrawalStatusPending,
		EstimatedFee: shared.MustNewDecimalFromString("0.0005"),
		NetAmount:    shared.MustNewDecimalFromString("9.9995"),
		CreatedAt:    shared.Timestamp{},
	}

	assert.NotEmpty(t, result.WithdrawalID)
	assert.NotNil(t, result.TxHash)
	assert.Equal(t, "0xtxhash...", *result.TxHash)
	assert.Equal(t, "0.0005", result.EstimatedFee.String())
	assert.Equal(t, "9.9995", result.NetAmount.String())
}

func TestWithdrawalStatus(t *testing.T) {
	pending := WithdrawalStatusPending
	broadcast := WithdrawalStatusBroadcast
	confirming := WithdrawalStatusConfirming
	completed := WithdrawalStatusCompleted
	failed := WithdrawalStatusFailed

	assert.Equal(t, "PENDING", pending.String())
	assert.Equal(t, "BROADCAST", broadcast.String())
	assert.Equal(t, "CONFIRMING", confirming.String())
	assert.Equal(t, "COMPLETED", completed.String())
	assert.Equal(t, "FAILED", failed.String())
}

func TestWithdrawalStatusType_String(t *testing.T) {
	assert.Equal(t, "PENDING", WithdrawalStatusPending.String())
	assert.Equal(t, "BROADCAST", WithdrawalStatusBroadcast.String())
	assert.Equal(t, "CONFIRMING", WithdrawalStatusConfirming.String())
	assert.Equal(t, "COMPLETED", WithdrawalStatusCompleted.String())
	assert.Equal(t, "FAILED", WithdrawalStatusFailed.String())
	assert.Equal(t, "CANCELLED", WithdrawalStatusCancelled.String())
	assert.Equal(t, "REJECTED", WithdrawalStatusRejected.String())
}

func TestFeeEstimate(t *testing.T) {
	estimate := &FeeEstimate{
		AssetID:       shared.NewUUID(),
		FeeLevel:      FeeLevelNormal,
		EstimatedFee:  shared.MustNewDecimalFromString("0.0005"),
		EstimatedTime: "10-30 minutes",
		MinFee:        shared.MustNewDecimalFromString("0.0003"),
		MaxFee:        shared.MustNewDecimalFromString("0.001"),
		UpdatedAt:     shared.Timestamp{},
	}

	assert.Equal(t, FeeLevelNormal, estimate.FeeLevel)
	assert.Equal(t, "0.0005", estimate.EstimatedFee.String())
	assert.Equal(t, "10-30 minutes", estimate.EstimatedTime)
}

func TestChainConfig(t *testing.T) {
	btc := BitcoinMainnet
	assert.Equal(t, "bitcoin", btc.Network)
	assert.Equal(t, "btc-mainnet", btc.ChainID)
	assert.Equal(t, "BTC", btc.NativeAsset)
	assert.Equal(t, 8, btc.NativeDecimals)
	assert.Equal(t, 6, btc.FinalityBlocks)
	assert.False(t, btc.SupportsTokens)
	assert.False(t, btc.RequiresTag)

	eth := EthereumMainnet
	assert.Equal(t, "ethereum", eth.Network)
	assert.Equal(t, "1", eth.ChainID)
	assert.Equal(t, "ETH", eth.NativeAsset)
	assert.Equal(t, 18, eth.NativeDecimals)
	assert.True(t, eth.SupportsTokens)
	assert.True(t, eth.SupportsEIP1559)
	assert.Equal(t, "ERC20", eth.TokenStandard)
	assert.Equal(t, "0x", eth.AddressPrefix)
	assert.False(t, eth.RequiresTag)

	usdtTrc20 := USDT_TRC20
	assert.Equal(t, "tron", usdtTrc20.Network)
	assert.Equal(t, "TRX", usdtTrc20.NativeAsset)
	assert.Equal(t, 6, usdtTrc20.NativeDecimals)
	assert.True(t, usdtTrc20.SupportsTokens)
	assert.Equal(t, "TRC20", usdtTrc20.TokenStandard)
	assert.Equal(t, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", usdtTrc20.ContractAddress)
	assert.Equal(t, "T", usdtTrc20.AddressPrefix)
	assert.Equal(t, 34, usdtTrc20.AddressLength)
}

func TestCustodyProviderType_String(t *testing.T) {
	assert.Equal(t, "INTERNAL", CustodyProviderInternal.String())
	assert.Equal(t, "BTC", CustodyProviderBTC.String())
	assert.Equal(t, "EVM", CustodyProviderEVM.String())
	assert.Equal(t, "SOLANA", CustodyProviderSolana.String())
	assert.Equal(t, "FIAT", CustodyProviderFiat.String())
}

func TestSettlementType_String(t *testing.T) {
	assert.Equal(t, "TRADE", SettlementTypeTrade.String())
	assert.Equal(t, "WITHDRAWAL", SettlementTypeWithdrawal.String())
	assert.Equal(t, "DEPOSIT", SettlementTypeDeposit.String())
	assert.Equal(t, "TRANSFER", SettlementTypeTransfer.String())
	assert.Equal(t, "FEE", SettlementTypeFee.String())
	assert.Equal(t, "FUNDING", SettlementTypeFunding.String())
	assert.Equal(t, "LIQUIDATION", SettlementTypeLiquidation.String())
}

func TestSettlementInstruction(t *testing.T) {
	instr := &SettlementInstruction{
		ID:         shared.NewUUID(),
		Type:       SettlementTypeTransfer,
		FromUserID: shared.NewUUID(),
		ToUserID:   shared.NewUUID(),
		AssetID:    shared.NewUUID(),
		Amount:     shared.MustNewDecimalFromString("100"),
		Mode:       shared.SettlementModeOffChain,
		Status:     SettlementStatusPending,
		CreatedAt:  shared.Timestamp{},
	}

	assert.Equal(t, SettlementTypeTransfer, instr.Type)
	assert.Equal(t, SettlementStatusPending, instr.Status)
}

func TestSettlementStatus_String(t *testing.T) {
	assert.Equal(t, "PENDING", SettlementStatusPending.String())
	assert.Equal(t, "PROCESSING", SettlementStatusProcessing.String())
	assert.Equal(t, "COMPLETED", SettlementStatusCompleted.String())
	assert.Equal(t, "FAILED", SettlementStatusFailed.String())
	assert.Equal(t, "CANCELLED", SettlementStatusCancelled.String())
}
