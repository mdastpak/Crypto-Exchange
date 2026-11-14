package assetregistry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateBitcoinAddress(t *testing.T) {
	for _, tc := range []struct{ address, canonical string }{
		{"1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}, // P2PKH, the genesis block
		{"3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy", "3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy"}, // P2SH, BIP 13
		// BIP 173 and BIP 350 test vectors.
		{"BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4", "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"},
		{"bc1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qccfmv3", "bc1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3qccfmv3"},
		{"bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0", "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0"},
	} {
		got, err := ValidateBitcoinAddress(tc.address)
		require.NoError(t, err, tc.address)
		assert.Equal(t, tc.canonical, got)
	}
	for name, address := range map[string]string{
		"bad Base58 checksum":       "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNb",
		"bad Bech32 checksum":       "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t5",
		"mixed case":                "bc1qW508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
		"testnet":                   "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		"version 1 with Bech32":     "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqh2y7hd", // BIP 350
		"version 0 with Bech32m":    "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kemeawh",                     // BIP 350
		"a TRON address":            "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		"an Ethereum address":       "0xdAC17F958D2ee523a2206206994597C13D831ec7",
		"empty":                     "",
		"non-Bech32 character (b)":  "bc1qb508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4",
		"Base58 with a zero (0)":    "10zP1eP5QGefi2DMPTfTL5SLmv7DivfNa",
		"too short after separator": "bc1qqqqq",
	} {
		_, err := ValidateBitcoinAddress(address)
		assert.Error(t, err, name)
	}
}

func TestNormalizeEthereumAddress(t *testing.T) {
	const checksummed = "0xdAC17F958D2ee523a2206206994597C13D831ec7"
	for _, typed := range []string{checksummed, "0xdac17f958d2ee523a2206206994597c13d831ec7", "0XDAC17F958D2EE523A2206206994597C13D831EC7"} {
		got, err := NormalizeEthereumAddress(typed)
		require.NoError(t, err, typed)
		assert.Equal(t, checksummed, got)
	}
	for _, bad := range []string{"0xdAC17F958D2ee523a2206206994597C13D831eC7", "0xdac17f958d2ee523a2206206994597c13d831e", "dac17f958d2ee523a2206206994597c13d831ec7xx", "0xzac17f958d2ee523a2206206994597c13d831ec7"} {
		_, err := NormalizeEthereumAddress(bad)
		assert.Error(t, err, bad)
	}
}
