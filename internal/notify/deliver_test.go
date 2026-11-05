package notify

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type deliverFunc func(ctx context.Context, msg OTPMessage) error

func (f deliverFunc) Deliver(ctx context.Context, msg OTPMessage) error { return f(ctx, msg) }

func validNotificationsConfig() config.NotificationsConfig {
	var cfg config.NotificationsConfig
	cfg.Email = config.EmailConfig{
		Host:       "smtp.example.com",
		Port:       587,
		From:       "Exchange <no-reply@example.com>",
		OTPSubject: config.DefaultOTPEmailSubject,
		OTPBody:    config.DefaultOTPEmailBody,
	}
	cfg.SMS.Provider = "kavenegar"
	cfg.SMS.Kavenegar = config.KavenegarConfig{
		APIKey:      "test-api-key",
		OTPTemplate: "signin",
		BaseURL:     config.DefaultKavenegarBaseURL,
	}
	return cfg
}

func TestChannels_For(t *testing.T) {
	email := deliverFunc(func(context.Context, OTPMessage) error { return nil })
	channels := Channels{Email: email}

	assert.NotNil(t, channels.For(ContactEmail))
	assert.Nil(t, channels.For(ContactMobile), "an unconfigured channel has no deliverer")
	assert.Nil(t, channels.For("username"))
	assert.False(t, channels.Empty())
	assert.True(t, Channels{}.Empty())
}

func TestNewChannels(t *testing.T) {
	channels, err := NewChannels(validNotificationsConfig(), time.Minute)
	require.NoError(t, err)
	assert.IsType(t, &EmailDeliverer{}, channels.Email)
	assert.IsType(t, &KavenegarDeliverer{}, channels.SMS)

	channels, err = NewChannels(config.NotificationsConfig{}, time.Minute)
	require.NoError(t, err)
	assert.True(t, channels.Empty(), "channels without configuration are disabled")
}

func TestNewChannels_RejectsInvalidConfiguration(t *testing.T) {
	cases := map[string]func(*config.NotificationsConfig){
		"unknown SMS provider":   func(c *config.NotificationsConfig) { c.SMS.Provider = "carrier-pigeon" },
		"missing API key":        func(c *config.NotificationsConfig) { c.SMS.Kavenegar.APIKey = "" },
		"missing SMS template":   func(c *config.NotificationsConfig) { c.SMS.Kavenegar.OTPTemplate = "" },
		"relative base URL":      func(c *config.NotificationsConfig) { c.SMS.Kavenegar.BaseURL = "api.kavenegar.com" },
		"invalid from address":   func(c *config.NotificationsConfig) { c.Email.From = "not an address" },
		"invalid port":           func(c *config.NotificationsConfig) { c.Email.Port = 70000 },
		"unparsable template":    func(c *config.NotificationsConfig) { c.Email.OTPBody = "{{.Code" },
		"unknown template field": func(c *config.NotificationsConfig) { c.Email.OTPBody = "{{.Password}}" },
		"multi-line subject":     func(c *config.NotificationsConfig) { c.Email.OTPSubject = "Code\n{{.Code}}" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validNotificationsConfig()
			mutate(&cfg)
			_, err := NewChannels(cfg, time.Minute)
			assert.Error(t, err)
		})
	}
}

func TestIsPermanent(t *testing.T) {
	assert.True(t, IsPermanent(permanent(errors.New("rejected"))))
	assert.True(t, IsPermanent(fmt.Errorf("wrapped: %w", permanent(errors.New("rejected")))))
	assert.False(t, IsPermanent(errors.New("timeout")))
}

func TestRedact_RemovesTheCodeAndTheContact(t *testing.T) {
	msg := OTPMessage{Contact: "Alice@Example.com", MaskedContact: "Ali*****@Example.com", Code: "482913"}
	err := errors.New("550 <alice@example.com> rejected the message with code 482913")

	text := redact(err, msg)

	assert.NotContains(t, text, "482913")
	assert.NotContains(t, text, "alice@example.com")
	assert.Contains(t, text, "Ali*****@Example.com", "the masked contact keeps the error useful")
}
