package user

import (
	"net/http"
	"testing"
	"time"

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

	plan, err := planChange(ContactChangeRequest{Action: contactAdd, Type: "mobile", Value: mobile.value}, []storedContact{email})
	require.NoError(t, err)
	assert.Equal(t, email, plan.account, "an addition is proven with the contact the user has")

	plan, err = planChange(ContactChangeRequest{Action: contactReplace, Type: "email", Value: "new@example.com"}, []storedContact{email, mobile})
	require.NoError(t, err)
	assert.Equal(t, email, plan.account, "a replacement is proven with the contact it replaces, by default")
	plan, err = planChange(ContactChangeRequest{Action: contactReplace, Type: "email", Value: "new@example.com", VerifyWith: "mobile"}, []storedContact{email, mobile})
	require.NoError(t, err)
	assert.Equal(t, mobile, plan.account)

	_, err = planChange(ContactChangeRequest{Action: contactAdd, Type: "email", Value: "x@example.com"}, []storedContact{email})
	assert.Equal(t, codeContactExists, refusal(t, err))
	_, err = planChange(ContactChangeRequest{Action: contactReplace, Type: "email", Value: "me@example.COM"}, []storedContact{email})
	assert.Equal(t, codeSameContact, refusal(t, err), "the same address in another case")
	_, err = planChange(ContactChangeRequest{Action: contactReplace, Type: "mobile", Value: mobile.value}, []storedContact{email})
	assert.Equal(t, codeContactNotFound, refusal(t, err))
	_, err = planChange(ContactChangeRequest{Action: contactRemove, Type: "email"}, []storedContact{email})
	assert.Equal(t, codeLastContact, refusal(t, err))
	_, err = planChange(ContactChangeRequest{Action: contactRemove, Type: "email", VerifyWith: "mobile"}, []storedContact{email})
	assert.Equal(t, codeLastContact, refusal(t, err))
	_, err = planChange(ContactChangeRequest{Action: contactAdd, Type: "mobile", Value: mobile.value, VerifyWith: "mobile"}, []storedContact{email})
	assert.Equal(t, codeContactNotFound, refusal(t, err), "the contact to verify with must be verified already")
}
