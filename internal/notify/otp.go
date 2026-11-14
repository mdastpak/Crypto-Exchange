// Package notify delivers messages to users.
package notify

import (
	"context"
	"errors"
	"log"
	"strings"
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
	// AntiPhishingCode is the user's anti-phishing phrase (#375), which
	// every email shows, or "" when they have none. It is set only for a
	// contact that the account has verified. Never log it.
	AntiPhishingCode string
	// Uses and AgeDays are, for a reminder to change the anti-phishing
	// code, the emails that have shown it and its age in days.
	Uses    int64
	AgeDays int
}

// The purposes of a message other than a sign-in code.
const (
	// PurposeWithdrawal is the purpose of a code that confirms a
	// withdrawal.
	PurposeWithdrawal = "withdrawal"
	// PurposeAntiPhishingReminder is the reminder to change the
	// anti-phishing code (#375). It carries no code.
	PurposeAntiPhishingReminder = "anti_phishing_reminder"
)

// MaskContact hides most of a contact, as the sign-in logs do: the first
// three characters stay visible, with the domain of an email address or the
// last three digits of a mobile number.
func MaskContact(contactType, value string) string {
	const hidden = "*****"
	if contactType == ContactEmail {
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
	switch msg.Purpose {
	case PurposeWithdrawal:
		what = "withdrawal confirmation code (" + msg.Details + ")"
	case PurposeAntiPhishingReminder:
		s.Logger.Printf("DEVELOPMENT ONLY: reminder to change the anti-phishing code for %s %s: shown in %d emails, %d days old",
			msg.ContactType, msg.MaskedContact, msg.Uses, msg.AgeDays)
		return nil
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
