// Package userauth holds the second step of a user's sign-in (#307): the
// account password, which a user proves after the code sent to a contact.
package userauth

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	// MaxPasswordBytes bounds the work of one hash. It is far above what a
	// person types or a password manager generates.
	MaxPasswordBytes = 256
)

// Hasher hashes passwords with a deliberately slow function. It is given to
// Passwords by the server, so that this package, which the sign-in routes
// import, holds no such function itself: a password is hashed only behind
// the checks of Passwords.
type Hasher interface {
	// Hash returns the encoded hash of password, with a new salt.
	Hash(ctx context.Context, password string) (string, error)
	// Verify reports whether password matches the encoded hash, and
	// whether the hash uses other settings than the current ones.
	Verify(ctx context.Context, encoded, password string) (match, outdated bool, err error)
}

// The refusals of a password that the policy does not accept.
var (
	ErrPasswordTooShort  = errors.New("the password is too short")
	ErrPasswordTooLong   = errors.New("the password is too long")
	ErrPasswordTooSimple = errors.New("the password is too simple")
)

// minDistinct is the least number of different characters in a password.
const minDistinct = 5

// CheckPolicy checks a new password: its length in characters, and that it
// is not a few characters repeated. It asks for no character classes,
// which make passwords harder to remember and not harder to guess (NIST SP
// 800-63B section 5.1.1.2).
func CheckPolicy(password string, minLength int) error {
	if len(password) > MaxPasswordBytes || !utf8.ValidString(password) || strings.ContainsRune(password, 0) {
		return ErrPasswordTooLong
	}
	if utf8.RuneCountInString(password) < minLength {
		return ErrPasswordTooShort
	}
	distinct := map[rune]struct{}{}
	for _, r := range password {
		distinct[r] = struct{}{}
	}
	if len(distinct) < minDistinct {
		return ErrPasswordTooSimple
	}
	return nil
}
