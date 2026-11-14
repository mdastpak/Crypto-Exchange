// Package confirm confirms operations with the user's second factor (#161).
// Every withdrawal is confirmed with a code before it runs: a code sent by
// SMS, a code sent by email, or a code of the user's authenticator app, as
// the user chooses.
//
// A confirmation is bound to its operation. The operation's digest, the
// SHA-256 of what it does (Digest), is stored with the confirmation, and
// the operation consumes it only with the same digest, in its own
// transaction. So a code confirms that withdrawal, with that amount,
// currency, and destination, and no other.
//
// A confirmation goes through these steps:
//
//  1. Start: the code goes out with a message that says what it confirms.
//  2. Confirm: the user's code. A wrong code counts against the
//     confirmation and against the code store's or the authenticator's own
//     limits.
//  3. Consume: the operation, once, before the confirmation expires.
//
// The rows are sealed (#139), so a confirmation marked confirmed in SQL
// runs nothing.
package confirm

import (
	"crypto/sha256"
	"errors"
	"slices"
	"strings"

	"github.com/mdastpak/crypto-exchange/internal/rowseal"
)

// Method is how a user confirms.
type Method string

// The methods, in the order that a user who chose none is offered them:
// the authenticator first, as it needs no delivery.
const (
	MethodTOTP  Method = "totp"
	MethodSMS   Method = "sms"
	MethodEmail Method = "email"
)

// Methods are the methods.
var Methods = []Method{MethodTOTP, MethodSMS, MethodEmail}

// Valid reports whether m is a method.
func (m Method) Valid() bool { return slices.Contains(Methods, m) }

// Purpose is what a confirmation is for.
type Purpose string

// PurposeWithdrawal confirms a withdrawal.
const PurposeWithdrawal Purpose = "withdrawal"

// Digest returns the digest of an operation of purpose: the SHA-256 of the
// purpose and the fields that define the operation, such as the amount,
// the currency, and the destination of a withdrawal, each separated by a
// NUL byte so that no two lists of fields give the same input.
func Digest(p Purpose, fields ...string) []byte {
	h := sha256.New()
	h.Write([]byte("crypto-exchange/confirm/1\x00" + string(p)))
	for _, f := range fields {
		h.Write([]byte{0})
		h.Write([]byte(f))
	}
	return h.Sum(nil)
}

// Errors of the confirmations.
var (
	// ErrNoMethod is a user who has none of the methods: no
	// authenticator, and no verified mobile number or email address that
	// a code can be sent to.
	ErrNoMethod = errors.New("the user has no method to confirm with")
	// ErrMethodUnavailable is a method that the user cannot use now.
	ErrMethodUnavailable = errors.New("the method is not available to the user")
	// ErrNotFound is no confirmation of the user with that ID.
	ErrNotFound = errors.New("no such confirmation")
	// ErrExpired is a confirmation past its time.
	ErrExpired = errors.New("the confirmation expired")
	// ErrInvalidCode is a wrong code.
	ErrInvalidCode = errors.New("the code is wrong")
	// ErrTooManyAttempts is a confirmation that took too many wrong codes,
	// or a code store or authenticator that is locked.
	ErrTooManyAttempts = errors.New("too many wrong codes")
	// ErrAlreadyConfirmed is a confirmation that was confirmed before.
	ErrAlreadyConfirmed = errors.New("the confirmation was confirmed already")
	// ErrNotConfirmed is a confirmation that its operation would consume
	// before the user confirmed it.
	ErrNotConfirmed = errors.New("the confirmation is not confirmed")
	// ErrConsumed is a confirmation that an operation consumed already.
	ErrConsumed = errors.New("the confirmation was used already")
	// ErrMismatch is a confirmation of another operation.
	ErrMismatch = errors.New("the confirmation is of another operation")
	// ErrUnverified is a confirmation that does not match its seal.
	ErrUnverified = rowseal.ErrUnverified
)

// mask hides most of a contact, as the sign-in logs do: the first three
// characters stay visible, with the domain of an email address or the last
// three digits of a mobile number.
func mask(contactType, value string) string {
	const hidden = "*****"
	if contactType == "email" {
		at := strings.LastIndex(value, "@")
		if at <= 0 {
			return hidden
		}
		return value[:min(3, at)] + hidden + value[at:]
	}
	if len(value) <= 6 {
		return hidden
	}
	return value[:3] + hidden + value[len(value)-3:]
}

// Namespace is the namespace of the codes of the confirmations in the code
// store, apart from the sign-in codes and the codes of contact changes.
const Namespace = "confirm"
