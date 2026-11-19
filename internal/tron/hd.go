package tron

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"golang.org/x/crypto/ripemd160" //nolint:staticcheck,gosec // G507: BIP32 names a parent key by its HASH160, which RIPEMD-160 is part of.
)

// The deposit addresses are derived by BIP32 from the extended public key
// of the account m/44'/195'/0' (SLIP-44 coin 195 is TRON): the address of
// index i is m/44'/195'/0'/0/i. Only public derivation is possible from an
// xpub, so the server derives every address and can sign for none; the
// signer, which holds the seed, derives the same keys.

// xpubVersion is the version of a serialized extended public key on
// mainnet ("xpub").
var xpubVersion = [4]byte{0x04, 0x88, 0xb2, 0x1e}

// ExtendedPublicKey is a BIP32 extended public key.
type ExtendedPublicKey struct {
	key       *secp256k1.PublicKey
	chainCode [32]byte
	depth     byte
	// parent is the fingerprint of the parent key, and index the index of
	// the key under it, which its serialization carries.
	parent [4]byte
	index  uint32
}

// ErrXPub is a string that is not an extended public key.
var ErrXPub = errors.New("tron: not an extended public key (xpub)")

// ParseXPub reads a base58check serialized xpub.
func ParseXPub(s string) (*ExtendedPublicKey, error) {
	raw, err := unBase58Check(s)
	if err != nil || len(raw) != 78 {
		return nil, ErrXPub
	}
	if [4]byte(raw[:4]) != xpubVersion {
		return nil, fmt.Errorf("%w: the version is not that of an xpub; an xprv is never given to the server", ErrXPub)
	}
	key, err := secp256k1.ParsePubKey(raw[45:78])
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrXPub, err)
	}
	x := &ExtendedPublicKey{key: key, depth: raw[4], index: binary.BigEndian.Uint32(raw[9:13])}
	copy(x.parent[:], raw[5:9])
	copy(x.chainCode[:], raw[13:45])
	return x, nil
}

// MaxIndex is the largest index of a non-hardened child.
const MaxIndex = 1<<31 - 1

// Child derives the non-hardened child i.
func (x *ExtendedPublicKey) Child(i uint32) (*ExtendedPublicKey, error) {
	if i > MaxIndex {
		return nil, fmt.Errorf("tron: index %d is hardened, which an xpub cannot derive", i)
	}
	mac := hmac.New(sha512.New, x.chainCode[:])
	mac.Write(x.key.SerializeCompressed())
	var index [4]byte
	binary.BigEndian.PutUint32(index[:], i)
	mac.Write(index[:])
	sum := mac.Sum(nil)

	var tweak secp256k1.ModNScalar
	if overflow := tweak.SetByteSlice(sum[:32]); overflow || tweak.IsZero() {
		return nil, fmt.Errorf("tron: child %d is invalid; use the next index", i)
	}
	var point, parent secp256k1.JacobianPoint
	secp256k1.ScalarBaseMultNonConst(&tweak, &point)
	x.key.AsJacobian(&parent)
	secp256k1.AddNonConst(&point, &parent, &point)
	if (point.X.IsZero() && point.Y.IsZero()) || point.Z.IsZero() {
		return nil, fmt.Errorf("tron: child %d is invalid; use the next index", i)
	}
	point.ToAffine()
	child := &ExtendedPublicKey{key: secp256k1.NewPublicKey(&point.X, &point.Y), depth: x.depth + 1,
		parent: fingerprint(x.key), index: i}
	copy(child.chainCode[:], sum[32:])
	return child, nil
}

// PublicKey is the public key.
func (x *ExtendedPublicKey) PublicKey() *secp256k1.PublicKey { return x.key }

// DepositAddress derives the address of index i of the account: m/.../0/i.
func (x *ExtendedPublicKey) DepositAddress(i uint32) (Address, error) {
	external, err := x.Child(0)
	if err != nil {
		return Address{}, err
	}
	child, err := external.Child(i)
	if err != nil {
		return Address{}, err
	}
	return AddressOf(child.key), nil
}

// DerivationPath is the path of the address of index i from the root.
func DerivationPath(i uint32) string { return fmt.Sprintf("m/44'/195'/0'/0/%d", i) }

// Derive implements the derivation of the deposit addresses of a network:
// the base58check address of index i, and its path.
func (x *ExtendedPublicKey) Derive(i uint32) (string, string, error) {
	a, err := x.DepositAddress(i)
	if err != nil {
		return "", "", err
	}
	return a.String(), DerivationPath(i), nil
}

// fingerprint is the first four bytes of the HASH160 of a public key.
func fingerprint(k *secp256k1.PublicKey) [4]byte {
	sha := sha256.Sum256(k.SerializeCompressed())
	h := ripemd160.New() //nolint:gosec // G406: BIP32 fixes HASH160 for the fingerprint, which identifies and protects nothing.
	h.Write(sha[:])
	return [4]byte(h.Sum(nil)[:4])
}

// String serializes x as an xpub.
func (x *ExtendedPublicKey) String() string {
	raw := make([]byte, 0, 78)
	raw = append(raw, xpubVersion[:]...)
	raw = append(raw, x.depth)
	raw = append(raw, x.parent[:]...)
	raw = binary.BigEndian.AppendUint32(raw, x.index)
	raw = append(raw, x.chainCode[:]...)
	raw = append(raw, x.key.SerializeCompressed()...)
	return base58Check(raw)
}
