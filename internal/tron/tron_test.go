package tron

import (
	"encoding/hex"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The address of a key is the prefix 0x41 and the 20 bytes that Ethereum
// would use: those of the key 1 are well known.
func TestAddressOf(t *testing.T) {
	var one secp256k1.ModNScalar
	one.SetInt(1)
	priv := secp256k1.NewPrivateKey(&one)
	a := AddressOf(priv.PubKey())
	assert.Equal(t, "417e5f4552091a69125d5dfcb7b8c2659029395bdf", a.Hex())
	back, err := ParseAddress(a.String())
	require.NoError(t, err)
	assert.Equal(t, a, back)
}

// The USDT contract, in its two forms.
func TestParseAddress(t *testing.T) {
	a, err := ParseAddress("TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	require.NoError(t, err)
	assert.Equal(t, "41a614f803b6fd780986a42c78ec9c7f77e6ded13c", a.Hex())
	b, err := ParseAddress("41a614f803b6fd780986a42c78ec9c7f77e6ded13c")
	require.NoError(t, err)
	assert.Equal(t, a, b)
	assert.Equal(t, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", b.String())

	for _, bad := range []string{"", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6u", "0x7e5f4552091a69125d5dfcb7b8c2659029395bdf",
		"1BoatSLRHtKNngkdXEeobR76b53LETtpyT", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj60"} {
		_, err := ParseAddress(bad)
		assert.ErrorIs(t, err, ErrAddress, bad)
	}
}

// BIP32 test vector 1: the public child 1 of m/0H is m/0H/1.
func TestChild_BIP32Vector(t *testing.T) {
	parent, err := ParseXPub("xpub68Gmy5EdvgibQVfPdqkBBCHxA5htiqg55crXYuXoQRKfDBFA1WEjWgP6LHhwBZeNK1VTsfTFUHCdrfp1bgwQ9xv5ski8PX9rL2dZXvgGDnw")
	require.NoError(t, err)
	child, err := parent.Child(1)
	require.NoError(t, err)
	assert.Equal(t, "03501e454bf00751f24b1b489aa925215d66af2234e3891c3b21a52bedb3cd711c",
		hex.EncodeToString(child.PublicKey().SerializeCompressed()))
	want, err := ParseXPub("xpub6ASuArnXKPbfEwhqN6e3mwBcDTgzisQN1wXN9BJcM47sSikHjJf3UFHKkNAWbWMiGj7Wf5uMash7SyYq527Hqck2AxYysAA7xmALppuCkwQ")
	require.NoError(t, err)
	assert.Equal(t, want.chainCode, child.chainCode)

	_, err = parent.Child(1 << 31)
	assert.Error(t, err, "a hardened index cannot be derived from an xpub")
	_, err = ParseXPub("xprv9s21ZrQH143K3QTDL4LXw2F7HEK3wJUD2nW2nRk4stbPy6cq3jPPqjiChkVvvNKmPGJxWUtg6LAF1GgQBmJ8LdTA8mPnHpWbrEKMnDaVRfd")
	assert.ErrorIs(t, err, ErrXPub, "an xprv is refused")

	a, err := parent.DepositAddress(0)
	require.NoError(t, err)
	b, err := parent.DepositAddress(1)
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
	again, err := parent.DepositAddress(0)
	require.NoError(t, err)
	assert.Equal(t, a, again, "deterministic")
	assert.Equal(t, "m/44'/195'/0'/0/7", DerivationPath(7))
}
