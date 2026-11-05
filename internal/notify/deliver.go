package notify

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
)

// Deliverer sends a sign-in code over one channel, such as email or SMS.
type Deliverer interface {
	Deliver(ctx context.Context, msg OTPMessage) error
}

// Channels holds the deliverer for each contact type. A nil deliverer means
// that the channel is not configured.
type Channels struct {
	Email Deliverer
	SMS   Deliverer
}

// For returns the deliverer for contactType, or nil when there is none.
func (c Channels) For(contactType string) Deliverer {
	switch contactType {
	case ContactEmail:
		return c.Email
	case ContactMobile:
		return c.SMS
	}
	return nil
}

// Empty reports whether no channel is configured.
func (c Channels) Empty() bool {
	return c.Email == nil && c.SMS == nil
}

// NewChannels builds the channels configured in cfg. codeTTL is the lifetime
// of sign-in codes, which email templates can show.
func NewChannels(cfg config.NotificationsConfig, codeTTL time.Duration) (Channels, error) {
	var channels Channels
	if cfg.Email.Host != "" {
		email, err := NewEmailDeliverer(cfg.Email, codeTTL)
		if err != nil {
			return Channels{}, fmt.Errorf("notifications.email: %w", err)
		}
		channels.Email = email
	}

	switch cfg.SMS.Provider {
	case "":
	case "kavenegar":
		sms, err := NewKavenegarDeliverer(cfg.SMS.Kavenegar)
		if err != nil {
			return Channels{}, fmt.Errorf("notifications.sms.kavenegar: %w", err)
		}
		channels.SMS = sms
	default:
		return Channels{}, fmt.Errorf("notifications.sms.provider: unsupported provider %q", cfg.SMS.Provider)
	}
	return channels, nil
}

// ErrUnsupportedContactType is returned for a contact type without a
// configured channel.
var ErrUnsupportedContactType = errors.New("no delivery channel for this contact type")

// PermanentError reports a delivery failure that retrying cannot fix, such as
// a rejected recipient or invalid provider credentials.
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }

func (e *PermanentError) Unwrap() error { return e.Err }

func permanent(err error) error {
	return &PermanentError{Err: err}
}

// IsPermanent reports whether err is a delivery failure that must not be retried.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// redact returns the text of err with the code and the raw contact removed, so
// that it can be logged. Providers may echo the recipient in their errors.
func redact(err error, msg OTPMessage) string {
	text := err.Error()
	if msg.Contact != "" {
		replacement := msg.MaskedContact
		if replacement == "" {
			replacement = "[contact]"
		}
		contact := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(msg.Contact))
		text = contact.ReplaceAllLiteralString(text, replacement)
	}
	if msg.Code != "" {
		text = strings.ReplaceAll(text, msg.Code, "[code]")
	}
	return text
}
