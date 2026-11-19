// Package tron is the TRON network for the custody of TRX and its TRC20
// tokens, such as USDT (#68): addresses, the public derivation of deposit
// addresses from an extended public key, and a client of TronGrid. It holds
// no private key: the deposit addresses are derived from the xpub of the
// account, and signing is the signer's (cmd/tron-signer).
package tron

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"golang.org/x/crypto/sha3"
)

// AddressPrefix is the first byte of a TRON address on mainnet and on the
// testnets.
const AddressPrefix = 0x41

// Address is a TRON address: the prefix and the last 20 bytes of the
// Keccak-256 of the uncompressed public key.
type Address [21]byte

// ErrAddress is a string that is not a TRON address.
var ErrAddress = errors.New("tron: not a TRON address")

// AddressOf returns the address of a public key.
func AddressOf(pub *secp256k1.PublicKey) Address {
	h := sha3.NewLegacyKeccak256()
	h.Write(pub.SerializeUncompressed()[1:])
	sum := h.Sum(nil)
	var a Address
	a[0] = AddressPrefix
	copy(a[1:], sum[12:])
	return a
}

// String is the base58check form, such as TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7.
func (a Address) String() string { return base58Check(a[:]) }

// Hex is the hex form that the node's API takes, such as 41...
func (a Address) Hex() string { return hex.EncodeToString(a[:]) }

// ParseAddress reads a base58check address, or the hex form of the API.
func ParseAddress(s string) (Address, error) {
	var a Address
	var raw []byte
	var err error
	if len(s) == 42 && strings.HasPrefix(strings.ToLower(s), "41") {
		raw, err = hex.DecodeString(s)
	} else {
		raw, err = unBase58Check(s)
	}
	if err != nil || len(raw) != len(a) || raw[0] != AddressPrefix {
		return a, ErrAddress
	}
	copy(a[:], raw)
	return a, nil
}

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func checksum(b []byte) []byte {
	first := sha256.Sum256(b)
	second := sha256.Sum256(first[:])
	return second[:4]
}

func base58Check(payload []byte) string {
	return base58(append(append([]byte{}, payload...), checksum(payload)...))
}

func base58(b []byte) string {
	n := new(big.Int).SetBytes(b)
	radix, mod := big.NewInt(58), new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, radix, mod)
		out = append(out, alphabet[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, alphabet[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

func unBase58Check(s string) ([]byte, error) {
	raw, err := unBase58(s)
	if err != nil || len(raw) < 5 {
		return nil, ErrAddress
	}
	payload, sum := raw[:len(raw)-4], raw[len(raw)-4:]
	if !bytes.Equal(checksum(payload), sum) {
		return nil, fmt.Errorf("%w: bad checksum", ErrAddress)
	}
	return payload, nil
}

func unBase58(s string) ([]byte, error) {
	n := new(big.Int)
	radix := big.NewInt(58)
	for _, c := range []byte(s) {
		i := strings.IndexByte(alphabet, c)
		if i < 0 {
			return nil, ErrAddress
		}
		n.Mul(n, radix)
		n.Add(n, big.NewInt(int64(i)))
	}
	out := n.Bytes()
	for _, c := range []byte(s) {
		if c != alphabet[0] {
			break
		}
		out = append([]byte{0}, out...)
	}
	return out, nil
}
