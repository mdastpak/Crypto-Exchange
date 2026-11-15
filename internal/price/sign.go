package price

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The integrity of the prices (#450). Redis is not trusted with a price:
// whoever can write to it could otherwise set the reference price of the
// OTC quotes and of the settlements of delistings. The aggregator, which
// has no database and so no data keys, signs each price with an Ed25519
// private key that only it holds; every server holds the public keys, so
// that it can verify a price but never make one. The id of a key is the
// start of the SHA-256 of its public key, so that two keys can be
// configured for a rotation.

// signaturePrefix starts the canonical encoding: the version of the
// encoding.
const signaturePrefix = "price/v1"

// ErrUnverifiedPrice is a price that is not signed by a known key: one
// written to Redis by anyone but an aggregator. It is a stale price to a
// consumer, which refuses it.
var ErrUnverifiedPrice = fmt.Errorf("price: the price is not signed by a known key: %w", ErrStale)

// The reasons that a verification fails, as the metric counts them.
const (
	ReasonUnsigned     = "unsigned"
	ReasonUnknownKey   = "unknown_key"
	ReasonMalformed    = "malformed"
	ReasonBadSignature = "bad_signature"
)

// KeyID returns the id of a public key: the hex of the first 8 bytes of
// its SHA-256.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// canonical returns the bytes that the signature of p covers: the version,
// the key id, and every field but the signature, in a fixed order and
// format, so that a change to any of them breaks the signature.
func canonical(p Price) []byte {
	var b strings.Builder
	line := func(s string) {
		b.WriteString(s)
		b.WriteByte('\n')
	}
	line(signaturePrefix)
	line(p.KeyID)
	line(p.Market)
	line(string(p.Status))
	line(p.Bid.String())
	line(p.Ask.String())
	line(p.Mid.String())
	for _, windows := range []map[string]string{texts(p.VWAP), texts(p.TWAP)} {
		keys := make([]string, 0, len(windows))
		for k := range windows {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + "=" + windows[k]
		}
		line(strings.Join(parts, ","))
	}
	line(strings.Join(p.Sources, ","))
	line(strings.Join(p.Outliers, ","))
	line(strconv.FormatInt(p.At.UnixNano(), 10))
	line(strconv.FormatInt(p.Updated.UnixNano(), 10))
	return []byte(b.String())
}

// texts returns the decimals of m as their texts.
func texts[D fmt.Stringer](m map[string]D) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v.String()
	}
	return out
}

// Signer signs the prices of an aggregator.
type Signer struct {
	id  string
	key ed25519.PrivateKey
}

// NewSigner returns the signer of key.
func NewSigner(key ed25519.PrivateKey) *Signer {
	return &Signer{id: KeyID(key.Public().(ed25519.PublicKey)), key: key}
}

// ID is the id of the key.
func (s *Signer) ID() string { return s.id }

// Sign returns p signed. A nil signer returns p as it is.
func (s *Signer) Sign(p Price) Price {
	if s == nil {
		return p
	}
	p.KeyID, p.Signature = s.id, ""
	p.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(s.key, canonical(p)))
	return p
}

// Verifier verifies the prices with the public keys of the aggregators.
type Verifier struct {
	keys map[string]ed25519.PublicKey
}

// NewVerifier returns a verifier of the prices that keys signed.
func NewVerifier(keys ...ed25519.PublicKey) *Verifier {
	v := &Verifier{keys: map[string]ed25519.PublicKey{}}
	for _, k := range keys {
		v.keys[KeyID(k)] = k
	}
	return v
}

// IDs returns the ids of the keys, sorted.
func (v *Verifier) IDs() []string {
	ids := make([]string, 0, len(v.keys))
	for id := range v.keys {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// Verify returns "" when a known key signed p, or why it did not.
func (v *Verifier) Verify(p Price) string {
	if p.KeyID == "" && p.Signature == "" {
		return ReasonUnsigned
	}
	key, ok := v.keys[p.KeyID]
	if !ok {
		return ReasonUnknownKey
	}
	sig, err := base64.RawStdEncoding.DecodeString(p.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return ReasonMalformed
	}
	if !ed25519.Verify(key, canonical(p), sig) {
		return ReasonBadSignature
	}
	return ""
}

// LoadSigningKey reads an Ed25519 private key from a PEM file in PKCS #8,
// as `openssl genpkey -algorithm ed25519` writes it.
func LoadSigningKey(path string) (ed25519.PrivateKey, error) {
	block, err := readPEM(path)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("price signing key %s: %w", path, err)
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("price signing key %s: not an Ed25519 key", path)
	}
	return ed, nil
}

// LoadVerifyKey reads an Ed25519 public key from a PEM file in PKIX, as
// `openssl pkey -pubout` writes it.
func LoadVerifyKey(path string) (ed25519.PublicKey, error) {
	block, err := readPEM(path)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("price verify key %s: %w", path, err)
	}
	ed, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("price verify key %s: not an Ed25519 key", path)
	}
	return ed, nil
}

func readPEM(path string) (*pem.Block, error) {
	// The path is the operator's setting, not input.
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("price key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("price key " + path + ": no PEM block")
	}
	return block, nil
}
