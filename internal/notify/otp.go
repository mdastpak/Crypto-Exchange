// Package notify delivers messages to users.
package notify

import (
	"context"
	"errors"
	"log"
)

// Contact types that sign-in codes can be delivered to.
const (
	ContactEmail  = "email"
	ContactMobile = "mobile"
)

// OTPMessage is a one-time sign-in code addressed to a contact.
type OTPMessage struct {
	ContactType   string // ContactEmail or ContactMobile
	Contact       string // the raw address; never log it
	MaskedContact string // safe to log
	Code          string // never log it outside development
	// Purpose is what the code is for: empty for a sign-in, or
	// PurposeWithdrawal for the confirmation of a withdrawal (#161), which
	// is sent with a message of its own, so that a code that moves funds
	// cannot pass for a sign-in code.
	Purpose string
	// Details describe what a confirmation code confirms, such as
	// "100 USDT to TR7NHq...gjLj6t", for the message. Never a full number.
	Details string
	// Amount and Destination are Details in two parts, such as "100 USDT"
	// and "TR7NHq...gjLj6t", each at most 25 characters, for templates
	// whose parameters are short (#371).
	Amount, Destination string
}

// PurposeWithdrawal is the purpose of a code that confirms a withdrawal.
const PurposeWithdrawal = "withdrawal"

// OTPSender delivers sign-in codes.
type OTPSender interface {
	// Supports reports whether codes can be delivered to contacts of
	// contactType, so that a request can be refused before a code is issued.
	Supports(contactType string) bool
	SendOTP(ctx context.Context, msg OTPMessage) error
}

func knownContactType(contactType string) bool {
	return contactType == ContactEmail || contactType == ContactMobile
}

// DiscardSender drops codes. Tests use it when they read codes from Redis.
type DiscardSender struct{}

func (DiscardSender) Supports(contactType string) bool { return knownContactType(contactType) }

func (DiscardSender) SendOTP(ctx context.Context, msg OTPMessage) error { return nil }

// DevLogSender writes codes to the application log so that sign-in can be
// tried locally without a provider. Codes in logs let anyone with log access
// sign in as any user, so it must never run in production.
type DevLogSender struct {
	Logger *log.Logger
}

func (DevLogSender) Supports(contactType string) bool { return knownContactType(contactType) }

func (s DevLogSender) SendOTP(ctx context.Context, msg OTPMessage) error {
	what := "sign-in code"
	if msg.Purpose == PurposeWithdrawal {
		what = "withdrawal confirmation code (" + msg.Details + ")"
	}
	s.Logger.Printf("DEVELOPMENT ONLY: %s for %s %s: %s", what, msg.ContactType, msg.MaskedContact, msg.Code)
	return nil
}

// ErrDevLogCodesInRelease is returned when development code logging is enabled
// in release mode.
var ErrDevLogCodesInRelease = errors.New("otp.auth.dev_log_codes must not be enabled in release mode: it writes sign-in codes to the log")

// CheckDevLogCodes refuses development code logging in release mode.
func CheckDevLogCodes(devLogCodes, releaseMode bool) error {
	if devLogCodes && releaseMode {
		return ErrDevLogCodesInRelease
	}
	return nil
}
