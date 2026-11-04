// Package totp implements time-based one-time passwords (RFC 6238) with the
// parameters that authenticator apps use by default: HMAC-SHA1, 6 digits,
// and 30-second time steps.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// Digits is the length of a code.
	Digits = 6
	// Period is the length of a time step.
	Period = 30 * time.Second
	// Skew is the number of steps before and after the current one that are
	// also accepted, to tolerate clock drift and typing time.
	Skew = 1
	// SecretSize is the size of a generated secret, the size of an HMAC-SHA1
	// key recommended by RFC 4226.
	SecretSize = 20
)

// GenerateSecret returns a new random secret.
func GenerateSecret() ([]byte, error) {
	secret := make([]byte, SecretSize)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("read random bytes: %w", err)
	}
	return secret, nil
}

// EncodeSecret returns secret in the base32 form that authenticator apps
// accept for manual entry.
func EncodeSecret(secret []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
}

// Step returns the time step of t.
func Step(t time.Time) int64 {
	return t.Unix() / int64(Period/time.Second)
}

// Code returns the code of secret for step.
func Code(secret []byte, step int64) string {
	return code(secret, step, Digits)
}

// code implements HOTP (RFC 4226) with step as the counter.
func code(secret []byte, step int64, digits int) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(counter[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	modulus := uint32(1)
	for i := 0; i < digits; i++ {
		modulus *= 10
	}
	return fmt.Sprintf("%0*d", digits, value%modulus)
}

// Verify checks code against the steps around now and returns the step it
// matched. Callers must reject a step that is not later than the last
// accepted one, so that a code cannot be used twice.
func Verify(secret []byte, code string, now time.Time) (int64, bool) {
	if len(code) != Digits {
		return 0, false
	}
	current := Step(now)
	for step := current - Skew; step <= current+Skew; step++ {
		if subtle.ConstantTimeCompare([]byte(Code(secret, step)), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// ProvisioningURI returns the otpauth URI that authenticator apps import,
// usually from a QR code.
func ProvisioningURI(issuer, account string, secret []byte) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	query := url.Values{}
	query.Set("secret", EncodeSecret(secret))
	query.Set("issuer", issuer)
	query.Set("algorithm", "SHA1")
	query.Set("digits", fmt.Sprint(Digits))
	query.Set("period", fmt.Sprint(int(Period/time.Second)))
	// Authenticator apps expect %20 rather than + for spaces.
	return "otpauth://totp/" + label + "?" + strings.ReplaceAll(query.Encode(), "+", "%20")
}
