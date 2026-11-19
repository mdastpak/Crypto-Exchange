package tron

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// The signer holds the seed, and derives from it the private keys that the
// server's xpub derives the public ones of: the deposit addresses of the
// account m/44'/195'/0', and the hot wallet, the first address of the
// account m/44'/195'/1', which the server cannot derive.

// Hardened is the first hardened index.
const Hardened = uint32(1) << 31

// ExtendedPrivateKey is a BIP32 extended private key. It lives only in the
// signer.
type ExtendedPrivateKey struct {
	key       *secp256k1.PrivateKey
	chainCode [32]byte
	depth     byte
	parent    [4]byte
	index     uint32
}

// ErrSeed is a seed that BIP32 cannot use.
var ErrSeed = errors.New("tron: the seed must be 16 to 64 bytes")

// MasterKey derives the master key of a BIP32 seed.
func MasterKey(seed []byte) (*ExtendedPrivateKey, error) {
	if len(seed) < 16 || len(seed) > 64 {
		return nil, ErrSeed
	}
	mac := hmac.New(sha512.New, []byte("Bitcoin seed"))
	mac.Write(seed)
	sum := mac.Sum(nil)
	var k secp256k1.ModNScalar
	if overflow := k.SetByteSlice(sum[:32]); overflow || k.IsZero() {
		return nil, fmt.Errorf("tron: the seed gives an invalid master key")
	}
	x := &ExtendedPrivateKey{key: secp256k1.NewPrivateKey(&k)}
	copy(x.chainCode[:], sum[32:])
	return x, nil
}

// Child derives child i, hardened from Hardened on.
func (x *ExtendedPrivateKey) Child(i uint32) (*ExtendedPrivateKey, error) {
	mac := hmac.New(sha512.New, x.chainCode[:])
	if i >= Hardened {
		mac.Write([]byte{0})
		k := x.key.Key.Bytes()
		mac.Write(k[:])
	} else {
		mac.Write(x.key.PubKey().SerializeCompressed())
	}
	var index [4]byte
	binary.BigEndian.PutUint32(index[:], i)
	mac.Write(index[:])
	sum := mac.Sum(nil)
	var tweak secp256k1.ModNScalar
	if overflow := tweak.SetByteSlice(sum[:32]); overflow {
		return nil, fmt.Errorf("tron: child %d is invalid; use the next index", i)
	}
	tweak.Add(&x.key.Key)
	if tweak.IsZero() {
		return nil, fmt.Errorf("tron: child %d is invalid; use the next index", i)
	}
	child := &ExtendedPrivateKey{key: secp256k1.NewPrivateKey(&tweak), depth: x.depth + 1,
		parent: fingerprint(x.key.PubKey()), index: i}
	copy(child.chainCode[:], sum[32:])
	return child, nil
}

// Path derives the key of a path of indexes from x.
func (x *ExtendedPrivateKey) Path(indexes ...uint32) (*ExtendedPrivateKey, error) {
	k := x
	for _, i := range indexes {
		var err error
		if k, err = k.Child(i); err != nil {
			return nil, err
		}
	}
	return k, nil
}

// Public is the extended public key of x: of the account m/44'/195'/0',
// the xpub that the server is given.
func (x *ExtendedPrivateKey) Public() *ExtendedPublicKey {
	return &ExtendedPublicKey{key: x.key.PubKey(), chainCode: x.chainCode, depth: x.depth, parent: x.parent, index: x.index}
}

// PrivateKey is the private key.
func (x *ExtendedPrivateKey) PrivateKey() *secp256k1.PrivateKey { return x.key }

// Account derives the key of account n of TRON: m/44'/195'/n'.
func (x *ExtendedPrivateKey) Account(n uint32) (*ExtendedPrivateKey, error) {
	return x.Path(44+Hardened, 195+Hardened, n+Hardened)
}

// Keys of the accounts of the custody.
const (
	// DepositAccount is the account of the deposit addresses.
	DepositAccount = 0
	// HotAccount is the account of the hot wallet, which pays the
	// withdrawals from its first address.
	HotAccount = 1
)

// AddressKey derives the key of address i of an account key: .../0/i.
func (x *ExtendedPrivateKey) AddressKey(i uint32) (*secp256k1.PrivateKey, error) {
	k, err := x.Path(0, i)
	if err != nil {
		return nil, err
	}
	return k.key, nil
}
