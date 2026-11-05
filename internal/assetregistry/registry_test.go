package assetregistry

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoad_TheRegistryFile loads config/assets.yaml, which must pass every
// check, and pins the contract addresses that the issuers publish.
func TestLoad_TheRegistryFile(t *testing.T) {
	registry, err := Load(filepath.Join("..", "..", "config", "assets.yaml"))
	require.NoError(t, err)

	contracts := make(map[string]string)
	networksOf := make(map[string][]string)
	for _, a := range registry.Assets {
		for _, n := range a.Networks {
			networksOf[a.Symbol] = append(networksOf[a.Symbol], n.Network)
			if n.ContractAddress != "" {
				contracts[a.Symbol+"/"+n.Network] = n.ContractAddress
			}
		}
	}
	assert.Equal(t, map[string]string{
		// https://tether.to/en/supported-protocols/
		"USDT/tron":     "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		"USDT/ethereum": "0xdAC17F958D2ee523a2206206994597C13D831ec7",
		// https://developers.circle.com/stablecoins/usdc-contract-addresses
		"USDC/ethereum": "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48",
		// https://etherscan.io/token/0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599
		"WBTC/ethereum": "0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599",
	}, contracts)
	assert.Equal(t, []string{"tron", "ethereum"}, networksOf["USDT"], "USDT is one asset on two networks")
	assert.Len(t, registry.Markets, 8)
}

func TestValidateEthereumAddress(t *testing.T) {
	// The test vectors of EIP-55, and real token contracts.
	for _, valid := range []string{
		"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
		"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
		"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
		"0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48",
		"0xdAC17F958D2ee523a2206206994597C13D831ec7",
	} {
		assert.NoError(t, ValidateEthereumAddress(valid), valid)
	}
	for name, invalid := range map[string]string{
		"the old, wrong USDC contract": "0xA0b86a33E6441b8C4C8C0C4C4C4C4C4C4C4C4C4C",
		"one letter in the wrong case": "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAeD",
		"all lower case, no checksum":  "0xdac17f958d2ee523a2206206994597c13d831ec7",
		"too short":                    "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeA",
		"no 0x":                        "5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed00",
		"not hex":                      "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAeZ",
	} {
		assert.Error(t, ValidateEthereumAddress(invalid), name)
	}
	err := ValidateEthereumAddress("0xdac17f958d2ee523a2206206994597c13d831ec7")
	assert.ErrorContains(t, err, "0xdAC17F958D2ee523a2206206994597C13D831ec7", "the error names the checksummed form")
}

func TestValidateTronAddress(t *testing.T) {
	assert.NoError(t, ValidateTronAddress("TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"))
	for name, invalid := range map[string]string{
		"one character changed": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6u",
		"not starting with T":   "AR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		"too short":             "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6",
		"not Base58 (0)":        "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj60",
		"an Ethereum address":   "0xdAC17F958D2ee523a2206206994597C13D831ec7",
	} {
		assert.Error(t, ValidateTronAddress(invalid), name)
	}
}

// validRegistry is a small registry that passes every check; the tests
// below break it in one place each.
const validRegistry = `
assets:
  - symbol: "USD"
    name: "US Dollar"
    type: "FIAT"
    status: "ACTIVE"
    decimals: 2
    networks:
      - network: "internal"
        min_withdrawal: "10"
        max_withdrawal: "0"
        withdrawal_fee: "1"
        custody_mode: "OFF_CHAIN"
  - symbol: "TRX"
    name: "TRON"
    type: "COIN"
    status: "ACTIVE"
    decimals: 6
    networks:
      - network: "tron"
        fee_asset: "TRX"
        min_withdrawal: "1"
        max_withdrawal: "100"
        withdrawal_fee: "1"
        custody_mode: "ON_CHAIN"
  - symbol: "USDT"
    name: "Tether USD"
    type: "STABLE"
    status: "ACTIVE"
    decimals: 6
    networks:
      - network: "tron"
        contract_address: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
        fee_asset: "TRX"
        min_withdrawal: "1"
        max_withdrawal: "100"
        withdrawal_fee: "1"
        custody_mode: "ON_CHAIN"
markets:
  - base: "TRX"
    quote: "USDT"
    status: "ACTIVE"
    min_order_size: "1"
    max_order_size: "0"
    min_notional: "1"
    tick_size: "0.0001"
    lot_size: "1"
    maker_fee_bps: 10
    taker_fee_bps: 20
`

func TestParse_RefusesBrokenRegistries(t *testing.T) {
	_, err := Parse([]byte(validRegistry))
	require.NoError(t, err, "the base registry is valid")

	usdtOnTron := `      - network: "tron"
        contract_address: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
        fee_asset: "TRX"
        min_withdrawal: "1"
        max_withdrawal: "100"
        withdrawal_fee: "1"
        custody_mode: "ON_CHAIN"
`
	for name, tc := range map[string]struct {
		old, new, want string
	}{
		"a symbol defined twice":   {"  - symbol: \"TRX\"", "  - symbol: \"USDT\"", "symbol USDT is defined twice"},
		"a network defined twice":  {usdtOnTron, usdtOnTron + strings.Replace(usdtOnTron, "TR7NHq", "TR7NHq", 1), "network tron is defined twice"},
		"a network in the symbol":  {`symbol: "USDT"`, `symbol: "USDT_TRC20"`, "a network goes in networks"},
		"a wrong TRON checksum":    {"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6u", "Base58Check checksum does not match"},
		"a token without contract": {"        contract_address: \"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t\"\n", "", "a token needs its contract_address"},
		"a native asset with contract": {"      - network: \"tron\"\n        fee_asset: \"TRX\"\n        min_withdrawal: \"1\"\n        max_withdrawal: \"100\"",
			"      - network: \"tron\"\n        contract_address: \"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t\"\n        fee_asset: \"TRX\"\n        min_withdrawal: \"1\"\n        max_withdrawal: \"100\"", "the native asset of a network"},
		"an unknown network": {`network: "internal"`, `network: "swift"`, `unknown network "swift"`},
		"fiat on a chain":    {`type: "FIAT"`, `type: "COIN"`, "fiat assets, and only they, use the internal network"},
		"an unknown type":    {`type: "STABLE"`, `type: "MEME"`, `unknown type "MEME"`},
		"an unknown status": {`status: "ACTIVE"
    decimals: 6
    networks:
      - network: "tron"
        contract_address`, `status: "LIVE"
    decimals: 6
    networks:
      - network: "tron"
        contract_address`, `unknown status "LIVE"`},
		"too many decimals":        {"decimals: 2", "decimals: 19", "decimals is 19"},
		"the wrong fee asset":      {"        contract_address: \"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t\"\n        fee_asset: \"TRX\"", "        contract_address: \"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t\"\n        fee_asset: \"USDT\"", "network fees on tron are paid in TRX"},
		"on-chain custody of fiat": {`custody_mode: "OFF_CHAIN"`, `custody_mode: "ON_CHAIN"`, "must be OFF_CHAIN on the internal network"},
		"minimum over the maximum": {`min_withdrawal: "1"
        max_withdrawal: "100"
        withdrawal_fee: "1"
        custody_mode: "ON_CHAIN"
  - symbol: "USDT"`, `min_withdrawal: "101"
        max_withdrawal: "100"
        withdrawal_fee: "1"
        custody_mode: "ON_CHAIN"
  - symbol: "USDT"`, "the minimum withdrawal 101 exceeds the maximum 100"},
		"a negative fee": {`withdrawal_fee: "1"
        custody_mode: "OFF_CHAIN"`, `withdrawal_fee: "-1"
        custody_mode: "OFF_CHAIN"`, `withdrawal fee "-1" must be a decimal of at least 0`},
		"a market of an unknown asset": {`base: "TRX"`, `base: "BTC"`, "base and quote must be assets of the registry"},
		"a market of one asset":        {`base: "TRX"`, `base: "USDT"`, "base and quote must differ"},
		"a zero tick size":             {`tick_size: "0.0001"`, `tick_size: "0"`, "tick_size must be positive"},
		"a fee of more than 10%":       {"taker_fee_bps: 20", "taker_fee_bps: 2000", "taker_fee_bps is 2000"},
		"a misspelled field":           {"withdrawal_fee: \"1\"\n        custody_mode: \"OFF_CHAIN\"", "withdrawl_fee: \"1\"\n        custody_mode: \"OFF_CHAIN\"", "field withdrawl_fee not found"},
	} {
		t.Run(name, func(t *testing.T) {
			require.Contains(t, validRegistry, tc.old, "the test must change the registry")
			_, err := Parse([]byte(strings.Replace(validRegistry, tc.old, tc.new, 1)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestParse_ReportsEveryProblem(t *testing.T) {
	broken := strings.NewReplacer(`type: "STABLE"`, `type: "MEME"`, `tick_size: "0.0001"`, `tick_size: "0"`).Replace(validRegistry)
	_, err := Parse([]byte(broken))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown type "MEME"`)
	assert.Contains(t, err.Error(), "tick_size must be positive")
}
