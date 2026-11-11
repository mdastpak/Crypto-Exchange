// Package passwordhash hashes account passwords with Argon2id. It is a
// deliberately slow function, so only the server wires it in, behind the
// checks of internal/userauth: the packages that serve unauthenticated
// requests must not import it.
package passwordhash

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argonParams are the cost settings of Argon2id. Each hash stores its own,
// so that they can be raised: a password hashed with others is hashed again
// at its next correct use.
type argonParams struct {
	// memory is in KiB.
	memory  uint32
	time    uint32
	threads uint8
}

// current follows the second recommendation of RFC 9106, for a server
// with limited memory: 64 MiB, three passes. It is a variable for the
// tests, which hash with less.
var current = argonParams{memory: 64 * 1024, time: 3, threads: 4}

const (
	saltLength = 16
	keyLength  = 32
)

// hashSlots limits the hashes in progress on this instance, each of which
// takes current.memory, so that a burst of sign-ins cannot use up the
// memory of the server.
var hashSlots = make(chan struct{}, 4)

// acquire waits for a hash slot.
func acquire(ctx context.Context) error {
	select {
	case hashSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func release() { <-hashSlots }

// Argon2id hashes passwords with Argon2id. Its hashes are PHC strings.
type Argon2id struct{}

// Hash returns the encoded Argon2id hash of password, with a new salt.
func (Argon2id) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, saltLength)
	_, _ = rand.Read(salt) // never fails since Go 1.24
	if err := acquire(ctx); err != nil {
		return "", err
	}
	defer release()
	p := current
	key := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, keyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.memory, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// ErrMalformedHash reports a stored hash that is not an Argon2id string.
var ErrMalformedHash = errors.New("the stored password hash is malformed")

// The most that a stored hash may ask for, so that a hash written by
// someone else cannot make a sign-in use unbounded memory or time.
const (
	maxMemory = 1024 * 1024
	maxTime   = 16
)

// parseHash reads an encoded hash.
func parseHash(encoded string) (p argonParams, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, ErrMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, ErrMalformedHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return p, nil, nil, ErrMalformedHash
	}
	if p.memory == 0 || p.memory > maxMemory || p.time == 0 || p.time > maxTime || p.threads == 0 {
		return p, nil, nil, ErrMalformedHash
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(salt) == 0 {
		return p, nil, nil, ErrMalformedHash
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(key) == 0 {
		return p, nil, nil, ErrMalformedHash
	}
	return p, salt, key, nil
}

// Verify reports whether password matches the encoded hash, and whether
// the hash uses other settings than the current ones.
func (Argon2id) Verify(ctx context.Context, encoded, password string) (match, outdated bool, err error) {
	p, salt, want, err := parseHash(encoded)
	if err != nil {
		return false, false, err
	}
	if err := acquire(ctx); err != nil {
		return false, false, err
	}
	defer release()
	got := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, uint32(len(want))) //nolint:gosec // G115: a key of some bytes.
	return subtle.ConstantTimeCompare(got, want) == 1, p != current || len(want) != keyLength, nil
}
