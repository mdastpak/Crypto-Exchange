package tron

import (
	"encoding/hex"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
)

var vector1Seed, _ = hex.DecodeString("000102030405060708090a0b0c0d0e0f")

// BIP32 test vector 1: the private derivation of m/0H gives the xpub that
// the server derives from, and m/0H/1 the same child.
func TestMasterKey_BIP32Vector(t *testing.T) {
	m, err := MasterKey(vector1Seed)
	require.NoError(t, err)
	assert.Equal(t, "0339a36013301597daef41fbe593a02cc513d0b55527ec2df1050e2e8ff49c85c2",
		hex.EncodeToString(m.PrivateKey().PubKey().SerializeCompressed()))
	h, err := m.Child(Hardened)
	require.NoError(t, err)
	want, err := ParseXPub("xpub68Gmy5EdvgibQVfPdqkBBCHxA5htiqg55crXYuXoQRKfDBFA1WEjWgP6LHhwBZeNK1VTsfTFUHCdrfp1bgwQ9xv5ski8PX9rL2dZXvgGDnw")
	require.NoError(t, err)
	assert.Equal(t, want.key.SerializeCompressed(), h.Public().key.SerializeCompressed())
	assert.Equal(t, want.chainCode, h.Public().chainCode)
	assert.Equal(t, "xpub68Gmy5EdvgibQVfPdqkBBCHxA5htiqg55crXYuXoQRKfDBFA1WEjWgP6LHhwBZeNK1VTsfTFUHCdrfp1bgwQ9xv5ski8PX9rL2dZXvgGDnw",
		h.Public().String(), "the standard serialization")
	child, err := want.Child(1)
	require.NoError(t, err)
	assert.Equal(t, "xpub6ASuArnXKPbfEwhqN6e3mwBcDTgzisQN1wXN9BJcM47sSikHjJf3UFHKkNAWbWMiGj7Wf5uMash7SyYq527Hqck2AxYysAA7xmALppuCkwQ",
		child.String())
	c, err := h.Child(1)
	require.NoError(t, err)
	assert.Equal(t, "03501e454bf00751f24b1b489aa925215d66af2234e3891c3b21a52bedb3cd711c",
		hex.EncodeToString(c.PrivateKey().PubKey().SerializeCompressed()))

	// The signer's key of each deposit address is that of the server's
	// address.
	for i := uint32(0); i < 3; i++ {
		k, err := h.AddressKey(i)
		require.NoError(t, err)
		a, err := want.DepositAddress(i)
		require.NoError(t, err)
		assert.Equal(t, a, AddressOf(k.PubKey()))
	}
	_, err = MasterKey(vector1Seed[:8])
	assert.ErrorIs(t, err, ErrSeed)
}

func fields(t *testing.T, b []byte) map[protowire.Number][]any {
	t.Helper()
	out := map[protowire.Number][]any{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		require.Positive(t, n)
		b = b[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			require.Positive(t, n)
			out[num] = append(out[num], v)
			b = b[n:]
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			require.Positive(t, n)
			out[num] = append(out[num], v)
			b = b[n:]
		default:
			t.Fatalf("unexpected wire type %d", typ)
		}
	}
	return out
}

func hotKey(t *testing.T) (*ExtendedPrivateKey, Address) {
	m, err := MasterKey(vector1Seed)
	require.NoError(t, err)
	acct, err := m.Account(HotAccount)
	require.NoError(t, err)
	k, err := acct.AddressKey(0)
	require.NoError(t, err)
	return acct, AddressOf(k.PubKey())
}

// A transfer of TRX and one of USDT, built and signed: the fields of the
// protocol, and a signature by the hot wallet.
func TestBuild(t *testing.T) {
	acct, hot := hotKey(t)
	key, _ := acct.AddressKey(0)
	to, _ := ParseAddress("TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7")
	usdt, _ := ParseAddress("TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t")
	ref := RefBlock{Number: 0x0102_0304, ID: "00000000010203040a0b0c0d0e0f1011aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	now := time.UnixMilli(1_700_000_000_000)

	tx, err := Build(Payment{From: hot, To: to, Amount: big.NewInt(1_500_000)}, ref, now)
	require.NoError(t, err)
	raw := fields(t, tx.Raw)
	assert.Equal(t, []byte{0x03, 0x04}, raw[1][0], "the last two bytes of the block number")
	assert.Equal(t, []byte{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11}, raw[4][0], "bytes 8 to 16 of the block id")
	assert.Equal(t, uint64(1_700_000_000_000+600_000), raw[8][0])
	assert.Equal(t, uint64(1_700_000_000_000), raw[14][0])
	assert.Nil(t, raw[18], "no fee limit for TRX")
	contract := fields(t, raw[11][0].([]byte))
	assert.Equal(t, uint64(1), contract[1][0])
	anyMsg := fields(t, contract[2][0].([]byte))
	assert.Equal(t, "type.googleapis.com/protocol.TransferContract", string(anyMsg[1][0].([]byte)))
	transfer := fields(t, anyMsg[2][0].([]byte))
	assert.Equal(t, hot[:], transfer[1][0])
	assert.Equal(t, to[:], transfer[2][0])
	assert.Equal(t, uint64(1_500_000), transfer[3][0])

	signed := tx.Sign(key)
	assert.Len(t, signed.Signature, 65)
	signer, err := signed.Signer()
	require.NoError(t, err)
	assert.Equal(t, hot, signer)
	assert.Len(t, signed.ID(), 64)
	whole := fields(t, signed.Encode())
	assert.Equal(t, signed.Raw, whole[1][0])
	assert.Equal(t, signed.Signature, whole[2][0])

	tx, err = Build(Payment{From: hot, To: to, Token: usdt, Amount: big.NewInt(25_000_000), FeeLimit: 30_000_000}, ref, now)
	require.NoError(t, err)
	raw = fields(t, tx.Raw)
	assert.Equal(t, uint64(30_000_000), raw[18][0])
	contract = fields(t, raw[11][0].([]byte))
	assert.Equal(t, uint64(31), contract[1][0])
	anyMsg = fields(t, contract[2][0].([]byte))
	assert.Equal(t, "type.googleapis.com/protocol.TriggerSmartContract", string(anyMsg[1][0].([]byte)))
	trigger := fields(t, anyMsg[2][0].([]byte))
	assert.Equal(t, usdt[:], trigger[2][0])
	data := trigger[4][0].([]byte)
	assert.Equal(t, "a9059cbb"+"000000000000000000000000"+hex.EncodeToString(to[1:])+
		"00000000000000000000000000000000000000000000000000000000017d7840", hex.EncodeToString(data))

	for _, bad := range []Payment{
		{From: hot, To: to, Amount: big.NewInt(0)},
		{From: hot, To: hot, Amount: big.NewInt(1)},
		{From: hot, To: to, Token: usdt, Amount: big.NewInt(1)},
		{From: hot, To: to, Amount: new(big.Int).Lsh(big.NewInt(1), 70)},
	} {
		_, err := Build(bad, ref, now)
		assert.ErrorIs(t, err, ErrPayment)
	}
	_, err = Build(Payment{From: hot, To: to, Amount: big.NewInt(1)}, RefBlock{Number: 1, ID: "zz"}, now)
	assert.ErrorIs(t, err, ErrPayment)
}
