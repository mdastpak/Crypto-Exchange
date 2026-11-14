package app

import (
	"context"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	signUpRoute     = "/api/v1/public/auth/sign-up"
	agreementsRoute = "/api/v1/user/agreements"
)

// withDocuments requires the legal documents of #316 in the given versions.
func withDocuments(terms, privacy string) {
	config.Config.Legal.Documents = []config.LegalDocument{
		{Kind: "terms", Version: terms, URL: "https://example.com/legal/terms"},
		{Kind: "privacy", Version: privacy},
	}
}

// verifyCode sends the code of a sign-in with what its holder accepted.
func (e *e2eEnv) verifyCode(t *testing.T, contact string, accept map[string]string) (int, e2eResponse) {
	t.Helper()
	status, resp := e.post(t, "/api/v1/public/auth", map[string]string{"username": contact}, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	body := map[string]any{"otp": e.sender.last(contact)}
	if accept != nil {
		body["accept"] = accept
	}
	return e.post(t, "/api/v1/public/auth/"+resp.Info["uuid"].(string), body, "")
}

// agreementRows returns the acceptances of the owner of a contact, as
// kind@version, with the country of the first.
func (e *e2eEnv) agreementRows(t *testing.T, contact string) (rows []string, country string) {
	t.Helper()
	result, err := e.db.QueryContext(context.Background(), `SELECT a.kind || '@' || a.version, COALESCE(a.country, ''), a.ip IS NOT NULL
		FROM user_agreements a JOIN user_contacts c ON c.user_hk = a.user_hk
		WHERE c.value = $1 ORDER BY a.kind, a.version`, contact)
	require.NoError(t, err)
	defer result.Close()
	for result.Next() {
		var row string
		var hasIP bool
		require.NoError(t, result.Scan(&row, &country, &hasIP))
		assert.True(t, hasIP, "an acceptance has the address of its request")
		rows = append(rows, row)
	}
	require.NoError(t, result.Err())
	return rows, country
}

func (e *e2eEnv) usersWith(t *testing.T, contact string) int {
	t.Helper()
	var n int
	require.NoError(t, e.db.QueryRow(`SELECT COUNT(*) FROM user_contacts WHERE value = $1`, contact).Scan(&n))
	return n
}

// TestSignUp_NeedsTheAcceptedDocuments checks #316: an account is created
// only with an acceptance of the current version of every document, and
// the acceptance is stored with the account.
func TestSignUp_NeedsTheAcceptedDocuments(t *testing.T) {
	env := newE2EEnv(t)
	withDocuments("2026-11", "v3")
	current := map[string]string{"terms": "2026-11", "privacy": "v3"}

	t.Run("accepted with the code", func(t *testing.T) {
		const email = "accepts@example.com"
		status, resp := env.verifyCode(t, email, current)
		require.Equal(t, http.StatusOK, status, resp.Message)
		require.NotEmpty(t, resp.Info["access_token"])
		rows, _ := env.agreementRows(t, email)
		assert.Equal(t, []string{"privacy@v3", "terms@2026-11"}, rows)

		var events int
		require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = $1`, audit.ActionUserAgreementAccept).Scan(&events))
		assert.Equal(t, 2, events, "each acceptance is in the audit log")

		// Signing in again asks nothing and records nothing.
		status, resp = env.signInUser(t, email)
		require.Equal(t, http.StatusOK, status, resp.Message)
		rows, _ = env.agreementRows(t, email)
		assert.Len(t, rows, 2)
	})

	t.Run("asked, then accepted with the challenge", func(t *testing.T) {
		const email = "asked@example.com"
		status, resp := env.verifyCode(t, email, nil)
		require.Equal(t, http.StatusPreconditionRequired, status, resp.Message)
		assert.Equal(t, "signup_terms_required", resp.Code)
		assert.Len(t, resp.Info["documents"], 2)
		challenge, _ := resp.Info["challenge"].(string)
		require.NotEmpty(t, challenge)
		assert.Zero(t, env.usersWith(t, email), "no account without the acceptance")

		// An old version is not an acceptance, and the challenge stays.
		status, resp = env.post(t, signUpRoute, map[string]any{"challenge": challenge, "accept": map[string]string{"terms": "2025-01", "privacy": "v3"}}, "")
		require.Equal(t, http.StatusPreconditionRequired, status, resp.Message)
		assert.Equal(t, challenge, resp.Info["challenge"])
		assert.Zero(t, env.usersWith(t, email))

		status, resp = env.post(t, signUpRoute, map[string]any{"challenge": challenge, "accept": current}, "")
		require.Equal(t, http.StatusOK, status, resp.Message)
		require.NotEmpty(t, resp.Info["access_token"])
		rows, _ := env.agreementRows(t, email)
		assert.Equal(t, []string{"privacy@v3", "terms@2026-11"}, rows)

		// The challenge created one account.
		status, resp = env.post(t, signUpRoute, map[string]any{"challenge": challenge, "accept": current}, "")
		assert.Equal(t, http.StatusUnauthorized, status, resp.Message)
		assert.Equal(t, "challenge_invalid", resp.Code)
		assert.Equal(t, 1, env.usersWith(t, email))
	})

	t.Run("a wrong challenge", func(t *testing.T) {
		status, resp := env.post(t, signUpRoute, map[string]any{"challenge": "no-such-challenge", "accept": current}, "")
		assert.Equal(t, http.StatusUnauthorized, status, resp.Message)
		status, _ = env.post(t, signUpRoute, map[string]any{"accept": current}, "")
		assert.Equal(t, http.StatusBadRequest, status)
	})
}

// TestSignUp_AsksNothingWithoutDocuments checks that a deployment without
// legal.documents signs up as before.
func TestSignUp_AsksNothingWithoutDocuments(t *testing.T) {
	env := newE2EEnv(t)
	const email = "plain@example.com"
	status, resp := env.signInUser(t, email)
	require.Equal(t, http.StatusOK, status, resp.Message)
	rows, _ := env.agreementRows(t, email)
	assert.Empty(t, rows)
	token := resp.Info["access_token"].(string)
	status, body := env.call(t, http.MethodPatch, "/api/v1/user/profile", map[string]string{"first_name": "Sam"}, token)
	assert.Equal(t, http.StatusOK, status, string(body))
}

// TestAgreements_ANewVersionClosesChanges checks that a user accepts a new
// version of a document before their next change, and that reading and
// accepting stay open.
func TestAgreements_ANewVersionClosesChanges(t *testing.T) {
	env := newE2EEnv(t)
	const email = "updated@example.com"
	withDocuments("2026-11", "v3")
	status, resp := env.verifyCode(t, email, map[string]string{"terms": "2026-11", "privacy": "v3"})
	require.Equal(t, http.StatusOK, status, resp.Message)
	token := resp.Info["access_token"].(string)
	change := func() (int, string) {
		status, body := env.call(t, http.MethodPatch, "/api/v1/user/profile", map[string]string{"first_name": "Sam"}, token)
		return status, string(body)
	}
	status, body := change()
	require.Equal(t, http.StatusOK, status, body)

	withDocuments("2027-01", "v3")
	status, body = change()
	require.Equal(t, http.StatusForbidden, status, body)
	assert.Contains(t, body, "terms_update_required")
	assert.Contains(t, body, "2027-01")

	status, _ = env.call(t, http.MethodGet, "/api/v1/user/profile", nil, token)
	assert.Equal(t, http.StatusOK, status, "reading stays open")
	status, raw := env.call(t, http.MethodGet, agreementsRoute, nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), `"pending":[{"kind":"terms","version":"2027-01"`)

	// A user signs in while a document is pending: that is how they
	// reach the acceptance.
	status, resp = env.signInUser(t, email)
	require.Equal(t, http.StatusOK, status, resp.Message)

	status, resp = env.post(t, agreementsRoute, map[string]any{"accept": map[string]string{"terms": "2026-11", "privacy": "v3"}}, token)
	assert.Equal(t, http.StatusConflict, status, "the old version is not an acceptance")
	assert.Equal(t, "terms_update_required", resp.Code)

	status, resp = env.post(t, agreementsRoute, map[string]any{"accept": map[string]string{"terms": "2027-01", "privacy": "v3"}}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Empty(t, resp.Info["pending"])
	rows, _ := env.agreementRows(t, email)
	assert.Equal(t, []string{"privacy@v3", "terms@2026-11", "terms@2027-01"}, rows, "the earlier acceptance is kept")

	status, body = change()
	assert.Equal(t, http.StatusOK, status, body)
	status, _ = env.post(t, agreementsRoute, map[string]any{}, "")
	assert.Equal(t, http.StatusUnauthorized, status)
}

// TestSignUp_BlockedCountries checks legal.signup.blocked_countries: a
// visitor from a listed country gets no new account, a user who has one
// still signs in, and the country of an acceptance is stored.
func TestSignUp_BlockedCountries(t *testing.T) {
	env := newE2EEnv(t)
	c := &config.Config
	c.GeoIP.Enabled = true
	// httptest sends from 192.0.2.1.
	c.GeoIP.Overrides = map[string]string{"192.0.2.0/24": "IR"}
	c.GeoIP.ReloadIntervalSeconds, c.GeoIP.MaxAgeDays = 300, 45
	geo, _, err := startGeoIP()
	require.NoError(t, err)
	env.handlers.Public.GeoIP, env.handlers.User.GeoIP = geo, geo
	withDocuments("2026-11", "v3")
	current := map[string]string{"terms": "2026-11", "privacy": "v3"}

	const member = "member@example.com"
	status, resp := env.verifyCode(t, member, current)
	require.Equal(t, http.StatusOK, status, resp.Message)
	_, country := env.agreementRows(t, member)
	assert.Equal(t, "IR", country)

	c.Legal.SignUp.BlockedCountries = []string{"KP", "IR"}
	const visitor = "visitor@example.com"
	status, resp = env.verifyCode(t, visitor, current)
	require.Equal(t, http.StatusForbidden, status, resp.Message)
	assert.Equal(t, "signup_country_unsupported", resp.Code)
	assert.Zero(t, env.usersWith(t, visitor))

	status, resp = env.signInUser(t, member)
	assert.Equal(t, http.StatusOK, status, "an existing account still signs in: %s", resp.Message)
}
