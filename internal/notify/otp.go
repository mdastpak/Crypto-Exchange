// Package notify delivers messages to users.
package notify

import (
	"context"
	"errors"
	"log"
)

// OTPMessage is a one-time sign-in code addressed to a contact.
type OTPMessage struct {
	ContactType   string // "email", "mobile" or "username"
	Contact       string // the raw address; never log it
	MaskedContact string // safe to log
	Code          string // never log it outside development
}

// OTPSender delivers sign-in codes. Email and SMS providers are tracked in #14.
type OTPSender interface {
	SendOTP(ctx context.Context, msg OTPMessage) error
}

// DiscardSender drops codes. It is the default until a delivery provider is
// configured.
type DiscardSender struct{}

func (DiscardSender) SendOTP(ctx context.Context, msg OTPMessage) error { return nil }

// DevLogSender writes codes to the application log so that sign-in can be
// tried locally without a provider. Codes in logs let anyone with log access
// sign in as any user, so it must never run in production.
type DevLogSender struct {
	Logger *log.Logger
}

func (s DevLogSender) SendOTP(ctx context.Context, msg OTPMessage) error {
	s.Logger.Printf("DEVELOPMENT ONLY: sign-in code for %s %s: %s", msg.ContactType, msg.MaskedContact, msg.Code)
	return nil
}

// ErrDevLogCodesInRelease is returned when development code logging is enabled
// in release mode.
var ErrDevLogCodesInRelease = errors.New("otp.auth.dev_log_codes must not be enabled in release mode: it writes sign-in codes to the log")

// NewOTPSender returns the sender selected by the configuration.
func NewOTPSender(devLogCodes, releaseMode bool) (OTPSender, error) {
	if !devLogCodes {
		return DiscardSender{}, nil
	}
	if releaseMode {
		return nil, ErrDevLogCodesInRelease
	}
	return DevLogSender{Logger: log.Default()}, nil
}
