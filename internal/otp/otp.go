// Package otp issues and verifies one-time sign-in codes.
package otp

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"encrypted-db/config"
)

// Defaults used for settings left at zero.
const (
	DefaultCodeLength             = 6
	DefaultTTL                    = 60 * time.Second
	DefaultAttemptsPerCode        = 3
	DefaultContactRequestsPerHour = 5
	DefaultContactAttemptsPerHour = 10
)

// MinSecretLength is the shortest accepted secret, in bytes.
const MinSecretLength = 32

// Settings configures a Store. Zero values select the defaults.
type Settings struct {
	// Namespace separates independent sign-in flows that share Redis, such
	// as user and admin sign-in: a session or a limit of one namespace is
	// unknown to the others. Empty is the user namespace.
	Namespace string
	// Secret keys the hashes of codes and contacts and the encryption of
	// contacts in Redis. Every server instance must use the same secret.
	Secret     string
	CodeLength int
	TTL        time.Duration
	// AttemptsPerCode bounds the verification attempts for one code.
	AttemptsPerCode int
	// ContactRequestsPerHour bounds the codes issued to one contact.
	ContactRequestsPerHour int
	// ContactAttemptsPerHour bounds the verification attempts for one
	// contact across all of its codes. A successful sign-in resets it.
	ContactAttemptsPerHour int
}

// SettingsFromConfig returns the settings in config.Config.OTP.Auth.
func SettingsFromConfig() Settings {
	cfg := config.Config.OTP.Auth
	return Settings{
		Secret:                 cfg.Secret,
		CodeLength:             cfg.Length,
		TTL:                    time.Duration(cfg.TTL) * time.Second,
		AttemptsPerCode:        cfg.RetryLimit,
		ContactRequestsPerHour: cfg.ContactRequestsPerHour,
		ContactAttemptsPerHour: cfg.ContactAttemptsPerHour,
	}
}

// WithDefaults returns the settings with zero values replaced by the defaults.
func (s Settings) WithDefaults() Settings {
	if s.CodeLength == 0 {
		s.CodeLength = DefaultCodeLength
	}
	if s.TTL == 0 {
		s.TTL = DefaultTTL
	}
	if s.AttemptsPerCode == 0 {
		s.AttemptsPerCode = DefaultAttemptsPerCode
	}
	if s.ContactRequestsPerHour == 0 {
		s.ContactRequestsPerHour = DefaultContactRequestsPerHour
	}
	if s.ContactAttemptsPerHour == 0 {
		s.ContactAttemptsPerHour = DefaultContactAttemptsPerHour
	}
	return s
}

// ErrSecretRequired is returned when no secret is configured in release mode.
var ErrSecretRequired = errors.New("otp.auth.secret (or OTP_SECRET) is required in release mode")

// Validate checks the settings after defaults are applied. An empty secret is
// accepted outside release mode, where NewStore uses a random one.
func (s Settings) Validate(releaseMode bool) error {
	s = s.WithDefaults()
	switch {
	case s.Secret == "" && releaseMode:
		return ErrSecretRequired
	case s.Secret != "" && len(s.Secret) < MinSecretLength:
		return fmt.Errorf("otp.auth.secret must be at least %d bytes", MinSecretLength)
	case s.CodeLength < 4 || s.CodeLength > 10:
		return fmt.Errorf("otp.auth.length must be between 4 and 10, got %d", s.CodeLength)
	case s.TTL < time.Second:
		return fmt.Errorf("otp.auth.ttl_seconds must be positive")
	case s.AttemptsPerCode < 1:
		return fmt.Errorf("otp.auth.retry_limit must be positive")
	case s.ContactRequestsPerHour < 1:
		return fmt.Errorf("otp.auth.contact_requests_per_hour must be positive")
	case s.ContactAttemptsPerHour < s.AttemptsPerCode:
		return fmt.Errorf("otp.auth.contact_attempts_per_hour must be at least otp.auth.retry_limit")
	}
	return nil
}

// GenerateCode returns length random decimal digits. Random bytes of 250 and
// above are discarded: 256 is not a multiple of 10, so mapping every byte
// with % 10 would make the digits 0 to 5 more likely than 6 to 9.
func GenerateCode(length int) (string, error) {
	if length <= 0 {
		return "", fmt.Errorf("invalid code length %d", length)
	}
	code := make([]byte, 0, length)
	random := make([]byte, length+length/4+1)
	for len(code) < length {
		if _, err := rand.Read(random); err != nil {
			return "", fmt.Errorf("read random bytes: %w", err)
		}
		for _, b := range random {
			if b >= 250 {
				continue
			}
			code = append(code, '0'+b%10)
			if len(code) == length {
				break
			}
		}
	}
	return string(code), nil
}
