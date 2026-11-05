package assetregistry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"

	"golang.org/x/crypto/sha3"
)

// ValidateEthereumAddress checks an Ethereum address: 0x and 40 hex digits,
// in the EIP-55 checksummed form, whose letter case encodes a checksum. An
// all-lower-case or all-upper-case address carries no checksum and is
// refused, so that a mistyped address cannot pass.
func ValidateEthereumAddress(address string) error {
	if len(address) != 42 || !strings.HasPrefix(address, "0x") {
		return errors.New("an Ethereum address is 0x followed by 40 hex digits")
	}
	hexPart := address[2:]
	if _, err := hex.DecodeString(hexPart); err != nil {
		return errors.New("an Ethereum address is 0x followed by 40 hex digits")
	}
	if want := checksumEthereumAddress(hexPart); address != want {
		return errors.New("the EIP-55 checksum does not match; the checksummed form is " + want)
	}
	return nil
}

// checksumEthereumAddress returns the EIP-55 form of an address: each letter
// is upper case where the matching nibble of the Keccak-256 hash of the lower
// case address is 8 or more.
func checksumEthereumAddress(hexPart string) string {
	lower := strings.ToLower(hexPart)
	hash := sha3.NewLegacyKeccak256()
	hash.Write([]byte(lower))
	digest := hex.EncodeToString(hash.Sum(nil))
	out := []byte("0x" + lower)
	for i := 0; i < len(lower); i++ {
		if lower[i] >= 'a' && digest[i] >= '8' {
			out[i+2] = lower[i] - 'a' + 'A'
		}
	}
	return string(out)
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// ValidateTronAddress checks a TRON address in Base58Check form: 34
// characters starting with T, which decode to the version byte 0x41, a
// 20-byte account, and a checksum, the first 4 bytes of the double SHA-256
// of the version and the account.
func ValidateTronAddress(address string) error {
	if len(address) != 34 || address[0] != 'T' {
		return errors.New("a TRON address is 34 Base58 characters starting with T")
	}
	decoded, err := decodeBase58(address)
	if err != nil || len(decoded) != 25 || decoded[0] != 0x41 {
		return errors.New("a TRON address is 34 Base58 characters starting with T")
	}
	first := sha256.Sum256(decoded[:21])
	second := sha256.Sum256(first[:])
	if !bytes.Equal(second[:4], decoded[21:]) {
		return errors.New("the Base58Check checksum does not match")
	}
	return nil
}

// decodeBase58 decodes Bitcoin-style Base58, keeping leading zero bytes.
func decodeBase58(s string) ([]byte, error) {
	n := new(big.Int)
	radix := big.NewInt(58)
	for _, c := range []byte(s) {
		digit := strings.IndexByte(base58Alphabet, c)
		if digit < 0 {
			return nil, errors.New("not Base58")
		}
		n.Mul(n, radix)
		n.Add(n, big.NewInt(int64(digit)))
	}
	decoded := n.Bytes()
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	return append(make([]byte, zeros), decoded...), nil
}
