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
}

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
	s.Logger.Printf("DEVELOPMENT ONLY: sign-in code for %s %s: %s", msg.ContactType, msg.MaskedContact, msg.Code)
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
