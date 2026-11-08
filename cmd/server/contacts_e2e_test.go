package main

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/restrictions"
	"github.com/mdastpak/crypto-exchange/internal/testutil/identitytest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// userOf returns the user that an access token names.
func userOf(t *testing.T, token string) string {
	t.Helper()
	claims, err := auth.ParseToken(token, auth.RoleUser, auth.TokenTypeAccess)
	require.NoError(t, err)
	return claims.UUID
}

// startChange starts a contact change and returns its id, with the codes
// that were sent to the account contact and to the new contact.
func (e *e2eEnv) startChange(t *testing.T, token string, body map[string]string, accountContact, newContact string) (id, accountCode, newCode string) {
	t.Helper()
	status, resp := e.post(t, "/api/v1/user/contacts/changes", body, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	id, _ = resp.Info["change_id"].(string)
	require.NotEmpty(t, id)
	accountCode = e.sender.last(accountContact)
	require.NotEmpty(t, accountCode)
	if newContact != "" {
		newCode = e.sender.last(newContact)
		require.NotEmpty(t, newCode)
		assert.NotEmpty(t, resp.Info["new_code_sent_to"])
	}
	return id, accountCode, newCode
}

func (e *e2eEnv) confirmChange(t *testing.T, token, id, accountCode, newCode string) (int, e2eResponse) {
	t.Helper()
	return e.post(t, "/api/v1/user/contacts/changes/"+id, map[string]string{"account_code": accountCode, "new_code": newCode}, token)
}

// contactTypes returns the types of the contacts that GET /user/contacts
// lists.
func (e *e2eEnv) contactTypes(t *testing.T, token string) []string {
	t.Helper()
	status, body := e.sessionCall(t, http.MethodGet, "/api/v1/user/contacts", token)
	require.Equal(t, http.StatusOK, status, body)
	var types []string
	for _, item := range body["info"].(map[string]interface{})["items"].([]interface{}) {
		ct := item.(map[string]interface{})
		types = append(types, ct["type"].(string))
		assert.Contains(t, ct["value"], "*****", "contacts are masked")
	}
	return types
}

// TestUserContacts_AddReplaceRemove walks a user through the contact
// changes of #199: a user who signed in by email adds a mobile number,
// signs in with it to the same account, replaces the email address, and
// removes the mobile number.
func TestUserContacts_AddReplaceRemove(t *testing.T) {
	env := newE2EEnv(t)
	const email, mobile, newEmail = "contacts@example.com", "09121110001", "renamed@example.com"
	token, _ := env.signedIn(t, email)
	userID := userOf(t, token)
	assert.Equal(t, []string{"email"}, env.contactTypes(t, token))

	// Add a mobile number. The account code goes to the email address.
	id, accountCode, newCode := env.startChange(t, token, map[string]string{"action": "add", "value": "+98 912 111 0001"}, email, mobile)
	status, resp := env.confirmChange(t, token, id, accountCode, "000000")
	require.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, "code_invalid", resp.Code)
	assert.Equal(t, "new_code", resp.Info["field"], "the account code was right")
	status, resp = env.confirmChange(t, token, id, "", newCode)
	require.Equal(t, http.StatusOK, status, "the accepted account code is not needed again: %s", resp.Message)
	assert.Equal(t, []string{"email", "mobile"}, env.contactTypes(t, token))

	mobileToken, _ := env.signedIn(t, mobile)
	assert.Equal(t, userID, userOf(t, mobileToken), "the mobile number signs in to the same account")

	// Replace the email address, proving the account with the mobile number,
	// as a user who lost the mailbox would.
	id, accountCode, newCode = env.startChange(t, token,
		map[string]string{"action": "replace", "value": newEmail, "verify_with": "mobile"}, mobile, newEmail)
	status, resp = env.confirmChange(t, token, id, accountCode, newCode)
	require.Equal(t, http.StatusOK, status, resp.Message)
	renamedToken, _ := env.signedIn(t, newEmail)
	assert.Equal(t, userID, userOf(t, renamedToken))
	oldToken, _ := env.signedIn(t, email)
	assert.NotEqual(t, userID, userOf(t, oldToken), "the old address no longer reaches the account")

	// Remove the mobile number; then the email address is the last one.
	id, accountCode, _ = env.startChange(t, token, map[string]string{"action": "remove", "type": "mobile"}, mobile, "")
	status, resp = env.confirmChange(t, token, id, accountCode, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Equal(t, []string{"email"}, env.contactTypes(t, token))
	status, resp = env.post(t, "/api/v1/user/contacts/changes", map[string]string{"action": "remove", "type": "email"}, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "last_contact", resp.Code)

	var actions []string
	rows, err := env.db.Query(`SELECT action FROM audit_events
		WHERE target_type = 'user' AND target_id = $1 AND action LIKE 'user.contact_%' AND outcome = 'success' ORDER BY hk`, userID)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var a string
		require.NoError(t, rows.Scan(&a))
		actions = append(actions, a)
	}
	assert.Equal(t, []string{audit.ActionUserContactAdd, audit.ActionUserContactReplace, audit.ActionUserContactRemove}, actions)
	var raw int
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM audit_events
		WHERE target_id = $1 AND (changes::text LIKE '%' || $2 || '%' OR changes::text LIKE '%' || $3 || '%')`,
		userID, mobile, newEmail).Scan(&raw))
	assert.Zero(t, raw, "audit events hold contacts masked")
}

func TestUserContacts_Refusals(t *testing.T) {
	env := newE2EEnv(t)
	token, _ := env.signedIn(t, "refusals@example.com")
	other, _ := env.signedIn(t, "other-user@example.com")

	for name, body := range map[string]map[string]string{
		"an unknown action":       {"action": "merge"},
		"an invalid value":        {"action": "add", "value": "not-a-contact"},
		"a type that is not that": {"action": "add", "value": "09121110002", "type": "email"},
		"a value for a removal":   {"action": "remove", "type": "email", "value": "x@example.com"},
	} {
		status, resp := env.post(t, "/api/v1/user/contacts/changes", body, token)
		assert.Equal(t, http.StatusBadRequest, status, name)
		assert.Equal(t, "validation_failed", resp.Code, name)
	}
	status, resp := env.post(t, "/api/v1/user/contacts/changes", map[string]string{"action": "add", "value": "another@example.com"}, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "contact_exists", resp.Code)
	status, resp = env.post(t, "/api/v1/user/contacts/changes", map[string]string{"action": "replace", "value": "09121110002"}, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "contact_not_found", resp.Code)

	// A change belongs to the user who started it.
	id, accountCode, newCode := env.startChange(t, token, map[string]string{"action": "add", "value": "09121110003"}, "refusals@example.com", "09121110003")
	status, resp = env.confirmChange(t, other, id, accountCode, newCode)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "change_not_found", resp.Code)
	status, _ = env.confirmChange(t, "", id, accountCode, newCode)
	assert.Equal(t, http.StatusUnauthorized, status)
	status, resp = env.confirmChange(t, token, id, accountCode, newCode)
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, _ = env.confirmChange(t, token, id, accountCode, newCode)
	assert.Equal(t, http.StatusNotFound, status, "a change applies once")
}

// TestUserContacts_ContactOfAnotherAccount adds a contact that another
// account holds. Starting the change does not tell that it is taken; once the
// code proves control of it, the refusal may.
func TestUserContacts_ContactOfAnotherAccount(t *testing.T) {
	env := newE2EEnv(t)
	const taken = "09121110004"
	env.signedIn(t, taken)
	token, _ := env.signedIn(t, "taker@example.com")

	id, accountCode, newCode := env.startChange(t, token, map[string]string{"action": "add", "value": taken}, "taker@example.com", taken)
	status, resp := env.confirmChange(t, token, id, accountCode, newCode)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "contact_in_use", resp.Code)
	assert.Equal(t, []string{"email"}, env.contactTypes(t, token))
}

// TestUserContacts_ConcurrentChangesApplyOneAtATime starts two additions of a
// mobile number and confirms them at once, as two instances would: the lock
// of the user's row applies one, and the other sees it and is refused.
func TestUserContacts_ConcurrentChangesApplyOneAtATime(t *testing.T) {
	env := newE2EEnv(t)
	const email = "racer@example.com"
	token, _ := env.signedIn(t, email)
	first, firstAccount, firstNew := env.startChange(t, token, map[string]string{"action": "add", "value": "09121110005"}, email, "09121110005")
	second, secondAccount, secondNew := env.startChange(t, token, map[string]string{"action": "add", "value": "09121110006"}, email, "09121110006")

	statuses := make([]int, 2)
	codes := make([]string, 2)
	var wg sync.WaitGroup
	for i, change := range [][3]string{{first, firstAccount, firstNew}, {second, secondAccount, secondNew}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var resp e2eResponse
			statuses[i], resp = env.confirmChange(t, token, change[0], change[1], change[2])
			codes[i] = resp.Code
		}()
	}
	wg.Wait()
	assert.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, statuses)
	assert.Contains(t, codes, "contact_exists")
	assert.Equal(t, []string{"email", "mobile"}, env.contactTypes(t, token))
}

// TestUserContacts_NewContactCooldown checks #250: a contact verified less
// than contacts.new_contact_cooldown_hours ago cannot replace or remove
// another contact, on start or on confirm, while a contact can always be
// changed with its own code.
func TestUserContacts_NewContactCooldown(t *testing.T) {
	env := newE2EEnv(t, func(c *config.Configuration) { c.Contacts.NewContactCooldownHours = 72 })
	const email, mobile = "cooldown@example.com", "09121110007"
	token, _ := env.signedIn(t, email)
	userID := userOf(t, token)

	// Adding is never held back, even by a contact verified a moment ago.
	id, accountCode, newCode := env.startChange(t, token, map[string]string{"action": "add", "value": mobile}, email, mobile)
	status, resp := env.confirmChange(t, token, id, accountCode, newCode)
	require.Equal(t, http.StatusOK, status, resp.Message)

	// The new mobile number cannot take the email address away.
	for _, body := range []map[string]string{
		{"action": "replace", "value": "attacker@example.com", "verify_with": "mobile"},
		{"action": "remove", "type": "email", "verify_with": "mobile"},
	} {
		status, resp = env.post(t, "/api/v1/user/contacts/changes", body, token)
		assert.Equal(t, http.StatusForbidden, status, body["action"])
		assert.Equal(t, "contact_too_new", resp.Code, body["action"])
		availableAt, err := time.Parse(time.RFC3339, resp.Info["available_at"].(string))
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(72*time.Hour), availableAt, time.Minute)
	}

	// Once the mobile number is older than the cooldown, it may. The time
	// is the database's: verified_at is moved back, not the clock.
	_, err := env.db.Exec(`UPDATE user_contacts SET verified_at = NOW() - interval '73 hours' WHERE user_hk = $1 AND type = 'mobile'`, userID)
	require.NoError(t, err)
	id, accountCode, newCode = env.startChange(t, token,
		map[string]string{"action": "replace", "value": "moved@example.com", "verify_with": "mobile"}, mobile, "moved@example.com")

	// The confirmation checks again: here the mobile number counts as new
	// again before the change is confirmed, as after a replacement.
	_, err = env.db.Exec(`UPDATE user_contacts SET verified_at = NOW() WHERE user_hk = $1 AND type = 'mobile'`, userID)
	require.NoError(t, err)
	status, resp = env.confirmChange(t, token, id, accountCode, newCode)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "contact_too_new", resp.Code)

	// The original contact can always undo the new one, and the new one can
	// be changed with its own code.
	id, accountCode, _ = env.startChange(t, token, map[string]string{"action": "remove", "type": "mobile", "verify_with": "email"}, email, "")
	status, resp = env.confirmChange(t, token, id, accountCode, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Equal(t, []string{"email"}, env.contactTypes(t, token))
	id, accountCode, newCode = env.startChange(t, token, map[string]string{"action": "replace", "value": "fresh@example.com"}, email, "fresh@example.com")
	status, resp = env.confirmChange(t, token, id, accountCode, newCode)
	require.Equal(t, http.StatusOK, status, resp.Message)
}

// TestUserContacts_ChangeStartsASecurityHold checks #251: an applied
// contact change holds withdrawals in its transaction, the user sees the
// hold, and the audit event records it.
func TestUserContacts_ChangeStartsASecurityHold(t *testing.T) {
	env := newE2EEnv(t)
	const email, mobile = "held@example.com", "09121110008"
	token, _ := env.signedIn(t, email)
	userID := userOf(t, token)

	status, body := env.sessionCall(t, http.MethodGet, "/api/v1/user/restrictions", token)
	require.Equal(t, http.StatusOK, status, body)
	assert.NotContains(t, body["info"], "security_hold_until", "signing up starts no hold")
	require.NoError(t, restrictions.Check(context.Background(), env.db, userID, restrictions.OpWithdraw, ""))

	id, accountCode, newCode := env.startChange(t, token, map[string]string{"action": "add", "value": mobile}, email, mobile)
	status, resp := env.confirmChange(t, token, id, accountCode, newCode)
	require.Equal(t, http.StatusOK, status, resp.Message)

	var refusal *restrictions.Refusal
	require.ErrorAs(t, restrictions.Check(context.Background(), env.db, userID, restrictions.OpWithdraw, ""), &refusal)
	assert.Equal(t, restrictions.CodeSecurityHold, refusal.Code)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), *refusal.Until, time.Minute)

	status, body = env.sessionCall(t, http.MethodGet, "/api/v1/user/restrictions", token)
	require.Equal(t, http.StatusOK, status, body)
	shown, err := time.Parse(time.RFC3339, body["info"].(map[string]interface{})["security_hold_until"].(string))
	require.NoError(t, err)
	assert.WithinDuration(t, *refusal.Until, shown, time.Second)

	var recorded string
	require.NoError(t, env.db.QueryRow(`SELECT changes->'after'->>'security_hold_until' FROM audit_events
		WHERE target_id = $1 AND action = $2 AND outcome = 'success'`, userID, audit.ActionUserContactAdd).Scan(&recorded))
	assert.NotEmpty(t, recorded)

	// The owner can still undo the change during the hold.
	id, accountCode, _ = env.startChange(t, token, map[string]string{"action": "remove", "type": "mobile", "verify_with": "email"}, email, "")
	status, resp = env.confirmChange(t, token, id, accountCode, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
}

// withNationalID gives the user a verified national ID, sealed with the
// test sealer that the handlers read it with, and a stub provider.
func (e *e2eEnv) withNationalID(t *testing.T, userID, nationalID string) *identity.Stub {
	t.Helper()
	sealer := identitytest.NewSealer()
	stub := &identity.Stub{}
	e.handlers.User.Sealer, e.handlers.User.Ownership = sealer, stub
	require.NoError(t, identity.SaveNationalID(context.Background(), e.db, sealer, userID, nationalID, "stub", "", time.Now()))
	return stub
}

// TestUserContacts_MobileOwnership checks #253 for a user with a verified
// national ID: a new mobile number must be registered to it, an outage
// changes nothing and lets the change be confirmed later, and the number
// cannot be removed.
func TestUserContacts_MobileOwnership(t *testing.T) {
	env := newE2EEnv(t)
	const email, foreign, own, other = "kyc@example.com", "09121110010", "09121110011", "09121110012"
	const nationalID = "0012345679"
	token, _ := env.signedIn(t, email)
	userID := userOf(t, token)
	stub := env.withNationalID(t, userID, nationalID)
	stub.SetOwner(foreign, "9876543210")
	stub.SetOwner(own, nationalID)
	stub.SetOwner(other, nationalID)

	// A number in someone else's name is refused once both codes are in,
	// and the change is gone.
	id, accountCode, newCode := env.startChange(t, token, map[string]string{"action": "add", "value": foreign}, email, foreign)
	status, resp := env.confirmChange(t, token, id, accountCode, newCode)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "mobile_owner_mismatch", resp.Code)
	assert.Equal(t, []string{"email"}, env.contactTypes(t, token))
	status, resp = env.confirmChange(t, token, id, accountCode, newCode)
	assert.Equal(t, http.StatusNotFound, status, resp.Message)
	var failures int
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM audit_events WHERE target_id = $1 AND reason = 'mobile_owner_mismatch'
		AND changes::text NOT LIKE '%' || $2 || '%'`, userID, foreign).Scan(&failures))
	assert.Equal(t, 1, failures, "the mismatch is audited, with the number masked")

	// An outage changes nothing; the same change confirms after it.
	id, accountCode, newCode = env.startChange(t, token, map[string]string{"action": "add", "value": own}, email, own)
	stub.SetUnavailable(true)
	status, resp = env.confirmChange(t, token, id, accountCode, newCode)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "identity_unavailable", resp.Code)
	assert.Equal(t, []string{"email"}, env.contactTypes(t, token))
	stub.SetUnavailable(false)
	status, resp = env.confirmChange(t, token, id, "", "")
	require.Equal(t, http.StatusOK, status, "the accepted codes are kept: %s", resp.Message)
	assert.Equal(t, []string{"email", "mobile"}, env.contactTypes(t, token))

	checkedRef := func() (bool, string) {
		var checked bool
		var ref string
		require.NoError(t, env.db.QueryRow(`SELECT ownership_checked_at IS NOT NULL, COALESCE(ownership_ref, '') FROM user_contacts
			WHERE user_hk = $1 AND type = 'mobile' AND removed_at IS NULL`, userID).Scan(&checked, &ref))
		return checked, ref
	}
	checked, ref := checkedRef()
	assert.True(t, checked)
	assert.NotEmpty(t, ref, "the provider's reference is kept for disputes")

	// The number cannot be removed, only replaced by another in the
	// user's name.
	status, resp = env.post(t, "/api/v1/user/contacts/changes", map[string]string{"action": "remove", "type": "mobile", "verify_with": "email"}, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "mobile_required", resp.Code)
	id, accountCode, newCode = env.startChange(t, token, map[string]string{"action": "replace", "value": other}, own, other)
	status, resp = env.confirmChange(t, token, id, accountCode, newCode)
	require.Equal(t, http.StatusOK, status, resp.Message)
	_, newRef := checkedRef()
	assert.NotEqual(t, ref, newRef, "the replacement was checked again")

	// Without a provider, a user with a national ID cannot change the
	// number: it cannot be checked.
	env.handlers.User.Ownership = nil
	id, accountCode, newCode = env.startChange(t, token, map[string]string{"action": "replace", "value": own}, other, own)
	status, resp = env.confirmChange(t, token, id, accountCode, newCode)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "identity_unavailable", resp.Code)

	// Emails are not checked.
	env.handlers.User.Ownership = stub
	calls := stub.Calls
	id, accountCode, newCode = env.startChange(t, token, map[string]string{"action": "replace", "value": "kyc2@example.com"}, email, "kyc2@example.com")
	status, resp = env.confirmChange(t, token, id, accountCode, newCode)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Equal(t, calls, stub.Calls)
}

// TestUserContacts_MobileOwnershipWaitsForTheNationalID checks the choice
// of #253 for level 0: a user without a verified national ID changes the
// mobile number with the codes only, and the provider is not asked.
func TestUserContacts_MobileOwnershipWaitsForTheNationalID(t *testing.T) {
	env := newE2EEnv(t)
	const email, mobile = "level0@example.com", "09121110013"
	token, _ := env.signedIn(t, email)
	stub := &identity.Stub{}
	env.handlers.User.Ownership, env.handlers.User.Sealer = stub, identitytest.NewSealer()

	id, accountCode, newCode := env.startChange(t, token, map[string]string{"action": "add", "value": mobile}, email, mobile)
	status, resp := env.confirmChange(t, token, id, accountCode, newCode)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Zero(t, stub.Calls)
	id, accountCode, _ = env.startChange(t, token, map[string]string{"action": "remove", "type": "mobile", "verify_with": "email"}, email, "")
	status, resp = env.confirmChange(t, token, id, accountCode, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
}
