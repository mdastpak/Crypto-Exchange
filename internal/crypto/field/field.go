// Package field encrypts personal fields in the application, so that the
// database holds only their ciphertext, and computes the blind indexes that
// find them by equality (#140).
//
// A sealed value is the version of the field-enc key in decimal, a colon, a
// random nonce, and the AES-256-GCM ciphertext with its tag (keys.SealField).
// Its associated data is its Place: the table, the column, and the primary
// key of its row, so that a value copied to another row or column, or
// changed, does not decrypt.
//
// A blind index is the version of the blind-index key in decimal, a colon,
// and the HMAC-SHA-256 of the normalized value under a domain, such as the
// contacts of one type. It reveals only whether two values are equal, so it
// is kept for the values that a lookup or a unique index needs.
//
// Neither the plaintext nor a part of it is ever in an error of this
// package.
package field

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"

	"github.com/mdastpak/crypto-exchange/internal/keys"
)

// Keys is what the package needs of the data keys. keys.Live and
// keys.Keyring both provide it.
type Keys interface {
	SealField(aad, plaintext []byte) ([]byte, error)
	OpenField(aad, sealed []byte) ([]byte, error)
	Current(purpose keys.Purpose) (keys.Key, error)
	Versions(purpose keys.Purpose) []keys.Key
}

// ErrNoCipher reports a seal or an index asked of a nil Cipher: the process
// has no data keys, which only development allows.
var ErrNoCipher = errors.New("no data keys to encrypt personal fields with")

// Place names where a value is stored, and is its associated data.
type Place struct {
	Table, Column string
	// Row is the primary key of the row, as text.
	Row string
}

// String is "table.column:row", the associated data of the value. Every
// sealed field of the code base uses this form.
func (p Place) String() string { return p.Table + "." + p.Column + ":" + p.Row }

// Cipher seals and opens fields, and computes blind indexes, with Keys. A
// nil *Cipher has no keys: it seals nothing and opens nothing.
type Cipher struct{ keys Keys }

// New returns a Cipher of k.
func New(k Keys) *Cipher { return &Cipher{keys: k} }

// active is the Cipher of the process, which the server sets when it has
// data keys.
var active atomic.Pointer[Cipher]

// Use makes c the Cipher of the process, and returns the function that
// puts the previous one back, for tests.
func Use(c *Cipher) (restore func()) {
	previous := active.Swap(c)
	return func() { active.Store(previous) }
}

// Active returns the Cipher of the process, or nil without data keys.
func Active() *Cipher { return active.Load() }

// Seal encrypts plaintext for p with the current field-enc key.
func (c *Cipher) Seal(p Place, plaintext string) ([]byte, error) {
	if c == nil {
		return nil, ErrNoCipher
	}
	sealed, err := c.keys.SealField([]byte(p.String()), []byte(plaintext))
	if err != nil {
		return nil, fmt.Errorf("seal %s: %w", p, err)
	}
	return sealed, nil
}

// Open decrypts a value that Seal sealed for p. A value that was changed,
// or sealed for another place, fails with keys.ErrSealedField.
func (c *Cipher) Open(p Place, sealed []byte) (string, error) {
	if c == nil {
		return "", ErrNoCipher
	}
	plaintext, err := c.keys.OpenField([]byte(p.String()), sealed)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", p, err)
	}
	return string(plaintext), nil
}

// SealOptional seals *v for p, and returns nil for a nil v, a column that
// stays NULL. A nil Cipher returns nil too: without data keys, which only
// development allows, the value is kept in plaintext only, until the
// rekey job seals it.
func (c *Cipher) SealOptional(p Place, v *string) ([]byte, error) {
	if c == nil || v == nil {
		return nil, nil
	}
	return c.Seal(p, *v)
}

// Read returns a field stored in plaintext, sealed, or both, as the first
// release of #140 writes it: the sealed value when there is one, else the
// plaintext. A sealed value that does not open is an error, never a reason
// to trust the plaintext. Without a Cipher, the plaintext is all there is
// to read; a value stored only sealed is then ErrNoCipher.
func (c *Cipher) Read(p Place, plain *string, sealed []byte) (*string, error) {
	if sealed == nil {
		return plain, nil
	}
	if c == nil {
		if plain != nil {
			return plain, nil
		}
		return nil, fmt.Errorf("open %s: %w", p, ErrNoCipher)
	}
	v, err := c.Open(p, sealed)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// indexTag starts what a blind index MACs, so that a blind-index key never
// MACs anything else by accident, and a future format can be told apart.
const indexTag = "crypto-exchange/blind-index/1"

// Index returns the blind index of value under domain, with the current
// blind-index key. value is already normalized: equal values must be equal
// strings.
func (c *Cipher) Index(domain, value string) ([]byte, error) {
	if c == nil {
		return nil, ErrNoCipher
	}
	key, err := c.keys.Current(keys.PurposeBlindIndex)
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", domain, err)
	}
	return index(key, domain, value)
}

// Indexes returns the blind index of value under domain with every loaded
// version of the blind-index key, newest first: what a lookup compares
// with, so that it also finds the rows indexed before a rotation, until
// they are indexed again. A nil Cipher returns none.
func (c *Cipher) Indexes(domain, value string) [][]byte {
	if c == nil {
		return [][]byte{}
	}
	versions := c.keys.Versions(keys.PurposeBlindIndex)
	out := make([][]byte, 0, len(versions))
	for _, key := range versions {
		if idx, err := index(key, domain, value); err == nil {
			out = append(out, idx)
		}
	}
	return out
}

// index computes the blind index of value under domain with key.
func index(key keys.Key, domain, value string) ([]byte, error) {
	h, err := key.NewMAC()
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", domain, err)
	}
	// The domain never holds a NUL byte, so the input is unambiguous.
	h.Write([]byte(indexTag + "\x00" + domain + "\x00" + value))
	return h.Sum([]byte(strconv.Itoa(key.Version()) + ":")), nil
}

// CurrentVersions returns the versions of the field-enc and blind-index
// keys that write now: a sealed value or an index of another version is
// sealed or indexed again by the rekey job.
func (c *Cipher) CurrentVersions() (sealing, indexing int, err error) {
	if c == nil {
		return 0, 0, ErrNoCipher
	}
	enc, err := c.keys.Current(keys.PurposeFieldEnc)
	if err != nil {
		return 0, 0, err
	}
	bidx, err := c.keys.Current(keys.PurposeBlindIndex)
	if err != nil {
		return 0, 0, err
	}
	return enc.Version(), bidx.Version(), nil
}

// Version returns the key version that a sealed value or a blind index
// names, and false when it names none.
func Version(b []byte) (int, bool) {
	colon := bytes.IndexByte(b, ':')
	if colon < 1 {
		return 0, false
	}
	v, err := strconv.Atoi(string(b[:colon]))
	if err != nil || v < 1 || strconv.Itoa(v) != string(b[:colon]) {
		return 0, false
	}
	return v, true
}

// Prefix returns the start of every sealed value or blind index of
// version, for a query that finds the values of other versions.
func Prefix(version int) []byte { return []byte(strconv.Itoa(version) + ":") }
