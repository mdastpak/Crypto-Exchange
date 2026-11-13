package assetregistry

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
)

// ValidateBitcoinAddress checks a Bitcoin mainnet address and returns its
// canonical form: a legacy Base58Check address, P2PKH (1...) or P2SH
// (3...), as given; or a SegWit address (bc1...) in lower case, Bech32 for
// witness version 0 (BIP 173) and Bech32m for later versions (BIP 350).
// Testnet addresses are refused.
func ValidateBitcoinAddress(address string) (string, error) {
	if strings.HasPrefix(strings.ToLower(address), "bc1") {
		return validateSegwit(address)
	}
	if len(address) < 26 || len(address) > 35 || (address[0] != '1' && address[0] != '3') {
		return "", errors.New("a Bitcoin address starts with 1, 3, or bc1")
	}
	decoded, err := decodeBase58(address)
	if err != nil || len(decoded) != 25 || (decoded[0] != 0x00 && decoded[0] != 0x05) {
		return "", errors.New("not a Bitcoin mainnet address")
	}
	first := sha256.Sum256(decoded[:21])
	second := sha256.Sum256(first[:])
	if !bytes.Equal(second[:4], decoded[21:]) {
		return "", errors.New("the Base58Check checksum does not match")
	}
	return address, nil
}

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

// The constants that a valid Bech32 and Bech32m checksum leaves.
const (
	bech32Const  = 1
	bech32mConst = 0x2bc830a3
)

func validateSegwit(address string) (string, error) {
	if len(address) > 90 || (address != strings.ToLower(address) && address != strings.ToUpper(address)) {
		return "", errors.New("a Bech32 address is in one case, at most 90 characters")
	}
	s := strings.ToLower(address)
	sep := strings.LastIndexByte(s, '1')
	if s[:sep] != "bc" || len(s)-sep-1 < 7 {
		return "", errors.New("not a Bitcoin mainnet address")
	}
	data := make([]byte, 0, len(s)-sep-1)
	for _, c := range []byte(s[sep+1:]) {
		v := strings.IndexByte(bech32Charset, c)
		if v < 0 {
			return "", errors.New("not a Bech32 character")
		}
		data = append(data, byte(v)) //nolint:gosec // G115: v indexes the 32 characters of the charset.
	}
	checksum := bech32Polymod(append(hrpExpand("bc"), data...))
	payload := data[:len(data)-6]
	version := payload[0]
	program, err := convertBits(payload[1:], 5, 8)
	if err != nil {
		return "", err
	}
	switch {
	case version == 0 && checksum == bech32Const && (len(program) == 20 || len(program) == 32):
	case version >= 1 && version <= 16 && checksum == bech32mConst && len(program) >= 2 && len(program) <= 40:
	default:
		return "", errors.New("the Bech32 checksum or the witness program does not match")
	}
	return s, nil
}

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := range 5 {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func hrpExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for _, c := range []byte(hrp) {
		out = append(out, c>>5)
	}
	out = append(out, 0)
	for _, c := range []byte(hrp) {
		out = append(out, c&31)
	}
	return out
}

// convertBits regroups 5-bit groups into bytes, refusing padding that is
// not zero or longer than a group.
func convertBits(data []byte, from, to uint) ([]byte, error) {
	var acc uint32
	var bits uint
	maxv := uint32(1)<<to - 1
	out := make([]byte, 0, len(data)*int(from)/int(to))
	for _, v := range data {
		acc = acc<<from | uint32(v)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxv)) //nolint:gosec // G115: maxv keeps it within a byte.
		}
	}
	if bits >= from || (acc<<(to-bits))&maxv != 0 {
		return nil, errors.New("the witness program has invalid padding")
	}
	return out, nil
}

// NormalizeEthereumAddress checks an Ethereum address that a user typed
// and returns its EIP-55 form. Unlike ValidateEthereumAddress, which the
// registry's contracts need checksummed, it accepts an address in one
// case, as many wallets show it; one in mixed case must match its
// checksum.
func NormalizeEthereumAddress(address string) (string, error) {
	if len(address) != 42 || (!strings.HasPrefix(address, "0x") && !strings.HasPrefix(address, "0X")) {
		return "", errors.New("an Ethereum address is 0x followed by 40 hex digits")
	}
	hexPart := address[2:]
	for _, c := range []byte(hexPart) {
		if !strings.ContainsRune("0123456789abcdefABCDEF", rune(c)) {
			return "", errors.New("an Ethereum address is 0x followed by 40 hex digits")
		}
	}
	want := checksumEthereumAddress(hexPart)
	if hexPart != strings.ToLower(hexPart) && hexPart != strings.ToUpper(hexPart) && "0x"+hexPart != want {
		return "", errors.New("the EIP-55 checksum does not match; the checksummed form is " + want)
	}
	return want, nil
}
