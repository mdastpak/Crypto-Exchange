package destinations

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// luhn completes the first 15 digits of a card number with its check
// digit.
func luhn(first15 string) string {
	sum := 0
	for i := range 15 {
		d := int(first15[14-i] - '0')
		if i%2 == 0 {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return first15 + fmt.Sprint((10-sum%10)%10)
}

// makeIBAN returns the Iranian IBAN of a basic account number of 22
// digits, with its check digits.
func makeIBAN(bban string) string {
	rem := 0
	for _, c := range []byte(bban + "182700") {
		rem = (rem*10 + int(c-'0')) % 97
	}
	return fmt.Sprintf("IR%02d%s", 98-rem, bban)
}

func TestNormalize_Cards(t *testing.T) {
	melli := luhn("603799123456789")
	n, err := normalize(Request{Kind: KindCard, Number: melli, Label: "  salary  "})
	require.NoError(t, err)
	assert.Equal(t, melli, n.Value)
	assert.Equal(t, "melli", n.Bank)
	assert.Equal(t, "salary", n.Label)
	assert.Equal(t, melli[:6]+"******"+melli[12:], n.Masked)
	assert.Equal(t, melli, n.Index)

	// The same number in groups and in Persian digits.
	persian := strings.NewReplacer("0", "۰", "1", "۱", "2", "۲", "3", "۳", "4", "۴", "5", "۵", "6", "۶", "7", "۷", "8", "۸", "9", "۹").
		Replace(melli[:4] + "-" + melli[4:8] + " " + melli[8:12] + "\u200c" + melli[12:])
	n2, err := normalize(Request{Kind: KindCard, Number: persian})
	require.NoError(t, err)
	assert.Equal(t, melli, n2.Value)

	unknown := luhn("999999123456789")
	n, err = normalize(Request{Kind: KindCard, Number: unknown})
	require.NoError(t, err, "a prefix that is not listed is still a card")
	assert.Empty(t, n.Bank)

	wrong := melli[:15] + fmt.Sprint((int(melli[15]-'0')+1)%10)
	for name, number := range map[string]string{"check digit": wrong, "15 digits": melli[:15], "letters": "60379912345678ab"} {
		_, err := normalize(Request{Kind: KindCard, Number: number})
		require.ErrorIs(t, err, ErrInvalid, name)
	}
}

func TestNormalize_IBANs(t *testing.T) {
	parsian := makeIBAN("0540102680020817909002")
	n, err := normalize(Request{Kind: KindIBAN, Number: strings.ToLower(parsian[:2]) + " " + parsian[2:]})
	require.NoError(t, err)
	assert.Equal(t, parsian, n.Value)
	assert.Equal(t, "parsian", n.Bank)
	assert.Equal(t, parsian[:7]+strings.Repeat("*", 15)+parsian[22:], n.Masked)

	n, err = normalize(Request{Kind: KindIBAN, Number: parsian[2:]})
	require.NoError(t, err, "IR may be left out")
	assert.Equal(t, parsian, n.Value)

	broken := parsian[:25] + fmt.Sprint((int(parsian[25]-'0')+1)%10)
	for name, number := range map[string]string{"check digits": broken, "another country": "DE" + parsian[2:], "short": parsian[:20]} {
		_, err := normalize(Request{Kind: KindIBAN, Number: number})
		require.ErrorIs(t, err, ErrInvalid, name)
	}
}

func TestNormalize_Accounts(t *testing.T) {
	n, err := normalize(Request{Kind: KindAccount, Number: "0123-456789-01", Bank: "mellat"})
	require.NoError(t, err)
	assert.Equal(t, "012345678901", n.Value)
	assert.Equal(t, "mellat", n.Bank)
	assert.Equal(t, "********8901", n.Masked)

	_, err = normalize(Request{Kind: KindAccount, Number: "012345678901"})
	require.ErrorIs(t, err, ErrInvalid, "an account needs its bank")
	_, err = normalize(Request{Kind: KindAccount, Number: "012345678901", Bank: "nowhere"})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = normalize(Request{Kind: KindAccount, Number: "1234", Bank: "mellat"})
	require.ErrorIs(t, err, ErrInvalid)
}

func TestNormalize_CryptoAddresses(t *testing.T) {
	n, err := normalize(Request{Kind: KindCrypto, Network: "ethereum", Address: " 0xdac17f958d2ee523a2206206994597c13d831ec7 "})
	require.NoError(t, err)
	assert.Equal(t, "0xdAC17F958D2ee523a2206206994597C13D831ec7", n.Value, "stored checksummed")
	assert.Equal(t, "ethereum\x000xdac17f958d2ee523a2206206994597c13d831ec7\x00", n.Index, "indexed in one case")
	assert.Equal(t, "0xdAC1...831ec7", n.Masked)

	n, err = normalize(Request{Kind: KindCrypto, Network: "tron", Address: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"})
	require.NoError(t, err)
	assert.Equal(t, "tron", n.Network)

	n, err = normalize(Request{Kind: KindCrypto, Network: "bitcoin", Address: "BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4"})
	require.NoError(t, err)
	assert.Equal(t, "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", n.Value)

	for name, r := range map[string]Request{
		"an address of another network": {Kind: KindCrypto, Network: "tron", Address: "0xdAC17F958D2ee523a2206206994597C13D831ec7"},
		"an unknown network":            {Kind: KindCrypto, Network: "solana", Address: "So11111111111111111111111111111111111111112"},
		"a memo where there is none":    {Kind: KindCrypto, Network: "tron", Address: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", Memo: "123"},
		"a number for an address":       {Kind: KindCrypto, Network: "tron", Number: "6037991234567893"},
		"an address for a card":         {Kind: KindCard, Number: luhn("603799123456789"), Address: "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"},
		"an unknown kind":               {Kind: "paypal", Number: "x"},
		"a long label":                  {Kind: KindCard, Number: luhn("603799123456789"), Label: strings.Repeat("ب", 51)},
	} {
		_, err := normalize(r)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
}

// TestBanks_TheTablesNameKnownBanks keeps the prefix and code tables in
// Banks, the list a user chooses from.
func TestBanks_TheTablesNameKnownBanks(t *testing.T) {
	for code, bank := range ibanBanks {
		assert.Contains(t, Banks, bank, "IBAN code %s", code)
		assert.Len(t, code, 3)
	}
	for prefix, bank := range cardBanks {
		assert.Contains(t, Banks, bank, "card prefix %s", prefix)
		assert.Len(t, prefix, 6)
	}
}
