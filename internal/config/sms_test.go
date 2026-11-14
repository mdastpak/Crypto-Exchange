package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSMSConfig_ProvidersAndTemplates(t *testing.T) {
	assert.Nil(t, SMSConfig{}.ProviderNames())
	assert.Equal(t, []string{"kavenegar"}, SMSConfig{Provider: "kavenegar"}.ProviderNames(), "one provider, as before #371")
	both := SMSConfig{Providers: []string{"smsir", "kavenegar"}}
	assert.Equal(t, []string{"smsir", "kavenegar"}, both.ProviderNames())

	assert.False(t, both.HasTemplate("withdrawal"))
	both.SMSIR.WithdrawalTemplateID = 7
	assert.True(t, both.HasTemplate("withdrawal"))
	assert.False(t, both.HasTemplate("anti_phishing_reminder"))
	both.Kavenegar.AntiPhishingReminderTemplate = "reminder"
	assert.True(t, both.HasTemplate("anti_phishing_reminder"))
	assert.False(t, SMSConfig{Kavenegar: KavenegarConfig{WithdrawalTemplate: "w"}}.HasTemplate("withdrawal"),
		"a provider that is not configured does not count")

	on := false
	assert.True(t, SMSDeliveryReportsConfig{}.On(), "the reports are on by default")
	assert.False(t, SMSDeliveryReportsConfig{Enabled: &on}.On())
	assert.True(t, SMSDeliveryReportsConfig{}.Resend())
	assert.False(t, SMSDeliveryReportsConfig{ResendFailed: &on}.Resend())
}
