package asset

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAsset_IsValid(t *testing.T) {
	asset := &Asset{
		ID:       shared.NewUUID(),
		Symbol:   "BTC",
		Name:     "Bitcoin",
		Type:     shared.AssetTypeCoin,
		Status:   shared.AssetStatusActive,
		Decimals: 8,
		Network:  "bitcoin",
	}
	assert.True(t, asset.IsValid())
}

func TestAsset_IsCrypto(t *testing.T) {
	btc := &Asset{Type: shared.AssetTypeCoin, Network: "bitcoin"}
	usdt := &Asset{Type: shared.AssetTypeToken, Network: "ethereum", ContractAddress: "0x..."}
	usd := &Asset{Type: shared.AssetTypeFiat}

	assert.True(t, btc.IsCrypto())
	assert.True(t, usdt.IsCrypto())
	assert.False(t, usd.IsCrypto())
}

func TestAsset_IsFiat(t *testing.T) {
	usd := &Asset{Type: shared.AssetTypeFiat}
	btc := &Asset{Type: shared.AssetTypeCoin}

	assert.True(t, usd.IsFiat())
	assert.False(t, btc.IsFiat())
}

func TestAsset_IsStablecoin(t *testing.T) {
	usdt := &Asset{Type: shared.AssetTypeStable}
	btc := &Asset{Type: shared.AssetTypeCoin}

	assert.True(t, usdt.IsStablecoin())
	assert.False(t, btc.IsStablecoin())
}

func TestAsset_RequiresTag(t *testing.T) {
	xrp := &Asset{Symbol: "XRP", RequiresTag: true}
	btc := &Asset{Symbol: "BTC", RequiresTag: false}

	assert.True(t, xrp.RequiresTag)
	assert.False(t, btc.RequiresTag)
}

func TestAsset_RoundQuantity(t *testing.T) {
	asset := &Asset{Decimals: 8}

	// Test rounding to 8 decimals
	qty := shared.MustNewDecimalFromString("1.00000000")
	rounded := asset.RoundQuantity(qty)
	assert.Equal(t, qty.String(), rounded.String())

	// Test with more precision than allowed - banker's rounding
	qty = shared.MustNewDecimalFromString("1.123456789")
	rounded = asset.RoundQuantity(qty)
	// Banker's rounding: 1.123456789 -> 1.12345679 (rounds up because next digit is 9)
	assert.Equal(t, "1.12345679", rounded.String())
}

func TestAsset_ValidateWithdrawal(t *testing.T) {
	asset := &Asset{
		MinWithdrawal:     shared.MustNewDecimalFromString("0.001"),
		MaxWithdrawal:     shared.MustNewDecimalFromString("1000"),
		WithdrawalFee:     shared.MustNewDecimalFromString("0.0005"),
		WithdrawalEnabled: true,
	}

	// Valid amount
	err := asset.ValidateWithdrawal(shared.MustNewDecimalFromString("1.0"))
	assert.NoError(t, err)

	// Below minimum
	err = asset.ValidateWithdrawal(shared.MustNewDecimalFromString("0.0001"))
	assert.Error(t, err)

	// Above maximum
	err = asset.ValidateWithdrawal(shared.MustNewDecimalFromString("2000"))
	assert.Error(t, err)

	// Disabled
	asset.WithdrawalEnabled = false
	err = asset.ValidateWithdrawal(shared.MustNewDecimalFromString("1.0"))
	assert.Error(t, err)
}

func TestAssetConfig_IsValid(t *testing.T) {
	config := &AssetConfig{
		AssetID:               shared.NewUUID(),
		ChainID:               "1",
		RPCEndpoints:          []string{"https://eth-mainnet.g.alchemy.com/v2/..."},
		ExplorerAPI:           "https://api.etherscan.io",
		ConfirmationsRequired: 12,
		GasLimit:              21000,
		GasPriceStrategy:      "eip1559",
		MinConfirmations:      3,
		RequiresTag:           false,
	}
	assert.True(t, config.IsValid())
}

func TestAssetConfig_IsValid_MissingRequired(t *testing.T) {
	config := &AssetConfig{
		AssetID: shared.NewUUID(),
		// Missing ChainID, RPCEndpoints, ExplorerAPI
	}
	assert.False(t, config.IsValid())
}

func TestAssetPair_Symbol(t *testing.T) {
	pair := &AssetPair{
		BaseAssetID:  shared.NewUUID(),
		QuoteAssetID: shared.NewUUID(),
		BaseAsset:    &Asset{Symbol: "BTC"},
		QuoteAsset:   &Asset{Symbol: "USDT"},
	}
	assert.Equal(t, "BTC/USDT", pair.Symbol())
}

func TestAssetPair_Symbol_MissingAssets(t *testing.T) {
	pair := &AssetPair{
		BaseAssetID:  shared.NewUUID(),
		QuoteAssetID: shared.NewUUID(),
	}
	assert.Equal(t, "UNKNOWN/UNKNOWN", pair.Symbol())
}

func TestAssetPair_IsValid(t *testing.T) {
	pair := &AssetPair{
		BaseAssetID:  shared.NewUUID(),
		QuoteAssetID: shared.NewUUID(),
	}
	assert.True(t, pair.IsValid())

	// Same base and quote
	baseID := shared.NewUUID()
	pair2 := &AssetPair{
		BaseAssetID:  baseID,
		QuoteAssetID: baseID,
	}
	assert.False(t, pair2.IsValid())

	// Empty base (zero UUID)
	pair3 := &AssetPair{
		BaseAssetID:  shared.UUID{},
		QuoteAssetID: shared.NewUUID(),
	}
	t.Logf("pair3.BaseAssetID.IsZero() = %v", pair3.BaseAssetID.IsZero())
	t.Logf("pair3.QuoteAssetID.IsZero() = %v", pair3.QuoteAssetID.IsZero())
	assert.False(t, pair3.IsValid())

	// Empty quote
	pair4 := &AssetPair{
		BaseAssetID:  shared.NewUUID(),
		QuoteAssetID: shared.UUID{},
	}
	assert.False(t, pair4.IsValid())
}

func TestAssetType_String(t *testing.T) {
	assert.Equal(t, "FIAT", shared.AssetTypeFiat.String())
	assert.Equal(t, "COIN", shared.AssetTypeCoin.String())
	assert.Equal(t, "TOKEN", shared.AssetTypeToken.String())
	assert.Equal(t, "STABLE", shared.AssetTypeStable.String())
	assert.Equal(t, "WRAPPED", shared.AssetTypeWrapped.String())
}

func TestAssetStatus_String(t *testing.T) {
	assert.Equal(t, "ACTIVE", shared.AssetStatusActive.String())
	assert.Equal(t, "INACTIVE", shared.AssetStatusInactive.String())
	assert.Equal(t, "DEPRECATED", shared.AssetStatusDeprecated.String())
	assert.Equal(t, "DELISTED", shared.AssetStatusDelisted.String())
}

func TestAsset_MustNewDecimalFromString(t *testing.T) {
	d := shared.MustNewDecimalFromString("123.456")
	assert.Equal(t, "123.456", d.String())

	// Should panic on invalid
	require.Panics(t, func() {
		shared.MustNewDecimalFromString("invalid")
	})
}

func TestAsset_RoundQuantity_EdgeValues(t *testing.T) {
	for _, tc := range []struct {
		decimals int
		qty      string
		want     string
	}{
		{0, "1234.6", "1235"}, // zero-decimal assets such as IRR round to whole units
		{0, "1234.4", "1234"},
		{2, "1.005", "1.01"},
		{8, "0.123456789", "0.12345679"},
		{-1, "1.5", "1.5"},     // invalid: unchanged
		{40000, "1.5", "1.5"},  // above what the database stores: unchanged
		{2, "-1.005", "-1.01"}, // rounds half away from zero
	} {
		a := &Asset{Decimals: tc.decimals}
		assert.Equal(t, tc.want, a.RoundQuantity(shared.MustNewDecimalFromString(tc.qty)).String(), "%d decimals, %s", tc.decimals, tc.qty)
	}
}

func TestAsset_ValidateWithdrawal_EdgeValues(t *testing.T) {
	base := Asset{WithdrawalEnabled: true, MinWithdrawal: shared.MustNewDecimalFromString("0.01"), MaxWithdrawal: shared.MustNewDecimalFromString("100")}
	for name, tc := range map[string]struct {
		change func(a *Asset)
		amount string
		want   error
	}{
		"within the limits":       {nil, "1", nil},
		"at the minimum":          {nil, "0.01", nil},
		"at the maximum":          {nil, "100", nil},
		"under the minimum":       {nil, "0.009", ErrWithdrawalBelowMinimum},
		"over the maximum":        {nil, "100.0001", ErrWithdrawalAboveMaximum},
		"no maximum (0)":          {func(a *Asset) { a.MaxWithdrawal = shared.MustNewDecimalFromString("0") }, "1000000", nil},
		"zero amount":             {nil, "0", ErrInvalidWithdrawalAmount},
		"negative amount":         {nil, "-1", ErrInvalidWithdrawalAmount},
		"zero amount, no minimum": {func(a *Asset) { a.MinWithdrawal = shared.MustNewDecimalFromString("0") }, "0", ErrInvalidWithdrawalAmount},
		"withdrawals disabled":    {func(a *Asset) { a.WithdrawalEnabled = false }, "1", ErrWithdrawalDisabled},
	} {
		a := base
		if tc.change != nil {
			tc.change(&a)
		}
		assert.Equal(t, tc.want, a.ValidateWithdrawal(shared.MustNewDecimalFromString(tc.amount)), name)
	}
}
