package user

import (
	"net/http"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateChangeRequest_NormalizesTheNewContact(t *testing.T) {
	req := ContactChangeRequest{Action: contactAdd, Value: " +98 ۹۱۲ ۱۲۳ ۴۵۶۷ "}
	require.Nil(t, validateChangeRequest(&req))
	assert.Equal(t, "09121234567", req.Value)
	assert.Equal(t, "mobile", req.Type)

	for name, req := range map[string]ContactChangeRequest{
		"action":      {Action: "merge"},
		"value":       {Action: contactReplace, Value: "someone"},
		"type":        {Action: contactRemove, Type: "username"},
		"verify_with": {Action: contactRemove, Type: "email", VerifyWith: "username"},
	} {
		errs := validateChangeRequest(&req)
		assert.Contains(t, errs, name)
	}
}

func TestPlanChange(t *testing.T) {
	now := time.Now()
	email := storedContact{typ: "email", value: "Me@Example.com", verifiedAt: now}
	mobile := storedContact{typ: "mobile", value: "09121234567", verifiedAt: now}
	refusal := func(t *testing.T, err error) string {
		t.Helper()
		var e *errChange
		require.ErrorAs(t, err, &e)
		assert.Equal(t, http.StatusConflict, e.status)
		return e.code
	}

	plan, err := planChange(cooldownOf(&config.Config), ContactChangeRequest{Action: contactAdd, Type: "mobile", Value: mobile.value}, []storedContact{email}, now)
	require.NoError(t, err)
	assert.Equal(t, email, plan.account, "an addition is proven with the contact the user has")

	plan, err = planChange(cooldownOf(&config.Config), ContactChangeRequest{Action: contactReplace, Type: "email", Value: "new@example.com"}, []storedContact{email, mobile}, now)
	require.NoError(t, err)
	assert.Equal(t, email, plan.account, "a replacement is proven with the contact it replaces, by default")
	plan, err = planChange(cooldownOf(&config.Config), ContactChangeRequest{Action: contactReplace, Type: "email", Value: "new@example.com", VerifyWith: "mobile"}, []storedContact{email, mobile}, now)
	require.NoError(t, err)
	assert.Equal(t, mobile, plan.account)

	_, err = planChange(cooldownOf(&config.Config), ContactChangeRequest{Action: contactAdd, Type: "email", Value: "x@example.com"}, []storedContact{email}, now)
	assert.Equal(t, codeContactExists, refusal(t, err))
	_, err = planChange(cooldownOf(&config.Config), ContactChangeRequest{Action: contactReplace, Type: "email", Value: "me@example.COM"}, []storedContact{email}, now)
	assert.Equal(t, codeSameContact, refusal(t, err), "the same address in another case")
	_, err = planChange(cooldownOf(&config.Config), ContactChangeRequest{Action: contactReplace, Type: "mobile", Value: mobile.value}, []storedContact{email}, now)
	assert.Equal(t, codeContactNotFound, refusal(t, err))
	_, err = planChange(cooldownOf(&config.Config), ContactChangeRequest{Action: contactRemove, Type: "email"}, []storedContact{email}, now)
	assert.Equal(t, codeLastContact, refusal(t, err))
	_, err = planChange(cooldownOf(&config.Config), ContactChangeRequest{Action: contactRemove, Type: "email", VerifyWith: "mobile"}, []storedContact{email}, now)
	assert.Equal(t, codeLastContact, refusal(t, err))
	_, err = planChange(cooldownOf(&config.Config), ContactChangeRequest{Action: contactAdd, Type: "mobile", Value: mobile.value, VerifyWith: "mobile"}, []storedContact{email}, now)
	assert.Equal(t, codeContactNotFound, refusal(t, err), "the contact to verify with must be verified already")
}

// TestPlanChange_NewContactCooldown covers #250 at both ends of the
// cooldown, with the time that the database read the contacts at.
func TestPlanChange_NewContactCooldown(t *testing.T) {
	saved := config.Config.Contacts
	t.Cleanup(func() { config.Config.Contacts = saved })
	config.Config.Contacts.NewContactCooldownHours = 72

	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	email := storedContact{typ: "email", value: "me@example.com", verifiedAt: now.Add(-365 * 24 * time.Hour)}
	fresh := storedContact{typ: "mobile", value: "09121234567", verifiedAt: now.Add(-71 * time.Hour)}
	both := []storedContact{email, fresh}

	for name, req := range map[string]ContactChangeRequest{
		"replace": {Action: contactReplace, Type: "email", Value: "new@example.com", VerifyWith: "mobile"},
		"remove":  {Action: contactRemove, Type: "email", VerifyWith: "mobile"},
	} {
		_, err := planChange(cooldownOf(&config.Config), req, both, now)
		var e *errChange
		require.ErrorAs(t, err, &e, name)
		assert.Equal(t, http.StatusForbidden, e.status, name)
		assert.Equal(t, codeContactTooNew, e.code, name)
		assert.Equal(t, "2026-11-08T13:00:00Z", e.info["available_at"], name)
	}

	for name, req := range map[string]ContactChangeRequest{
		"the old contact replaces the new one": {Action: contactReplace, Type: "mobile", Value: "09129999999", VerifyWith: "email"},
		"the old contact removes the new one":  {Action: contactRemove, Type: "mobile", VerifyWith: "email"},
		"the new contact replaces itself":      {Action: contactReplace, Type: "mobile", Value: "09129999999"},
		"the new contact removes itself":       {Action: contactRemove, Type: "mobile"},
	} {
		_, err := planChange(cooldownOf(&config.Config), req, both, now)
		assert.NoError(t, err, name)
	}
	_, err := planChange(cooldownOf(&config.Config), ContactChangeRequest{Action: contactRemove, Type: "mobile", VerifyWith: "email"},
		[]storedContact{
			{typ: "email", value: "me@example.com", verifiedAt: now.Add(-2 * time.Hour)},
			{typ: "mobile", value: "09121234567", verifiedAt: now.Add(-time.Hour)},
		}, now)
	assert.NoError(t, err, "an older contact changes a newer one, even when both are recent")
	_, err = planChange(cooldownOf(&config.Config), ContactChangeRequest{Action: contactAdd, Type: "email", Value: "x@example.com"}, []storedContact{fresh}, now)
	assert.NoError(t, err, "adding is never held back")

	fresh.verifiedAt = now.Add(-72 * time.Hour)
	_, err = planChange(cooldownOf(&config.Config), ContactChangeRequest{Action: contactRemove, Type: "email", VerifyWith: "mobile"}, []storedContact{email, fresh}, now)
	assert.NoError(t, err, "the cooldown ends exactly after its hours")
}
