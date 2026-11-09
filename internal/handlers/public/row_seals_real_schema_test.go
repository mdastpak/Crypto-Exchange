package public

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/keys"
	"github.com/mdastpak/crypto-exchange/internal/models"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dataKeys loads the data keys of env's database, as a server does.
func (e *sessionTestEnv) dataKeys(t *testing.T) *keys.Live {
	t.Helper()
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	provider, err := keys.NewLocalProvider(base64.StdEncoding.EncodeToString(kek))
	require.NoError(t, err)
	live, err := keys.NewLive(context.Background(), &keys.Store{DB: e.conn, Provider: provider}, keys.LiveOptions{})
	require.NoError(t, err)
	return live
}

// sealWith seals the rows of env's database with live in mode, as a server
// with data keys does (#139).
func (e *sessionTestEnv) sealWith(t *testing.T, live *keys.Live, mode string) *rowseal.Sealer {
	t.Helper()
	sealer := rowseal.New(e.conn, live, mode)
	sealer.Logger = log.New(io.Discard, "", 0)
	t.Cleanup(rowseal.Use(sealer))
	return sealer
}

func (e *sessionTestEnv) useRowSeals(t *testing.T, mode string) *rowseal.Sealer {
	t.Helper()
	return e.sealWith(t, e.dataKeys(t), mode)
}

// accessTokenOf returns the access token of a sign-in or refresh answer.
func accessTokenOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp models.APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Info.(map[string]interface{})["access_token"].(string)
}

func (e *sessionTestEnv) openFindings(t *testing.T) []rowseal.Finding {
	t.Helper()
	findings, err := rowseal.ListFindings(context.Background(), e.conn, rowseal.ListOptions{})
	require.NoError(t, err)
	return findings
}

func TestRowSeals_RealSchema_EveryWriteOfASignInIsSealed(t *testing.T) {
	env := newSessionTestEnv(t)
	sealer := env.useRowSeals(t, rowseal.ModeEnforce)
	const email = "seals-flow@example.com"

	access, cookie, _ := env.signIn(t, email)
	require.Equal(t, http.StatusOK, env.send(http.MethodGet, "/user/ping", nil, withAuthorization(access)).Code)
	w := env.send(http.MethodPost, "/public/auth/refresh", nil, withRefreshCookie(cookie.Value))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	access = accessTokenOf(t, w)
	require.Equal(t, http.StatusOK, env.send(http.MethodGet, "/user/ping", nil, withAuthorization(access)).Code)
	_, _, _ = env.signIn(t, email) // a second sign-in finds the sealed contact
	require.Equal(t, http.StatusOK, env.send(http.MethodPost, "/public/auth/logout", nil, withAuthorization(access)).Code)

	// The verifier finds nothing that the backend wrote unsealed.
	report, err := sealer.VerifyAll(context.Background())
	require.NoError(t, err)
	assert.Positive(t, report.Checked)
	assert.Zero(t, report.Findings, "%+v", env.openFindings(t))
}

func TestRowSeals_RealSchema_EnforceRefusesRowsChangedInSQL(t *testing.T) {
	t.Run("a revoked session set back to active", func(t *testing.T) {
		env := newSessionTestEnv(t)
		env.useRowSeals(t, rowseal.ModeEnforce)
		const email = "seals-session@example.com"
		access, cookie, _ := env.signIn(t, email)
		require.Equal(t, http.StatusOK, env.send(http.MethodPost, "/public/auth/logout", nil, withAuthorization(access)).Code)
		_, err := env.conn.Exec(`UPDATE sessions SET status = 'ACTIVE', revoked_at = NULL`)
		require.NoError(t, err)

		assert.Equal(t, http.StatusUnauthorized, env.send(http.MethodPost, "/public/auth/refresh", nil, withRefreshCookie(cookie.Value)).Code)
		findings := env.openFindings(t)
		require.Len(t, findings, 1)
		assert.Equal(t, "sessions", findings[0].Table)
	})

	t.Run("a KYC tier raised in SQL", func(t *testing.T) {
		env := newSessionTestEnv(t)
		env.useRowSeals(t, rowseal.ModeEnforce)
		const email = "seals-user@example.com"
		access, _, _ := env.signIn(t, email)
		_, err := env.conn.Exec(`UPDATE users SET kyc_tier = 'ENHANCED'`)
		require.NoError(t, err)

		assert.Equal(t, http.StatusForbidden, env.send(http.MethodGet, "/user/ping", nil, withAuthorization(access)).Code)
		findings := env.openFindings(t)
		require.Len(t, findings, 1)
		assert.Equal(t, "users", findings[0].Table)
	})

	t.Run("a contact moved to another account", func(t *testing.T) {
		env := newSessionTestEnv(t)
		env.useRowSeals(t, rowseal.ModeEnforce)
		env.signIn(t, "seals-victim@example.com")
		env.signIn(t, "seals-attacker@example.com")
		// The attacker's address now signs in to the victim's account.
		_, err := env.conn.Exec(`UPDATE user_contacts SET removed_at = NOW() WHERE value = 'seals-victim@example.com'`)
		require.NoError(t, err)
		_, err = env.conn.Exec(`UPDATE user_contacts SET user_hk = (SELECT user_hk FROM user_contacts WHERE value = 'seals-victim@example.com')
			WHERE value = 'seals-attacker@example.com'`)
		require.NoError(t, err)

		signIn, code := env.requestCode(t, "seals-attacker@example.com")
		w := env.send(http.MethodPost, "/public/auth/"+signIn, map[string]string{"otp": code})
		assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		tables := map[string]bool{}
		for _, f := range env.openFindings(t) {
			tables[f.Table] = true
		}
		assert.True(t, tables["user_contacts"], "the moved contact is a finding")
	})
}

func TestRowSeals_RealSchema_ReportModeKeepsRowsOfAnEarlierReleaseWorking(t *testing.T) {
	env := newSessionTestEnv(t)
	// Signed in before the release that seals rows.
	t.Cleanup(rowseal.Use(nil))
	access, cookie, _ := env.signIn(t, "seals-rollout@example.com")

	live := env.dataKeys(t)
	sealer := env.sealWith(t, live, rowseal.ModeReport)
	require.Equal(t, http.StatusOK, env.send(http.MethodGet, "/user/ping", nil, withAuthorization(access)).Code)
	assert.NotEmpty(t, env.openFindings(t), "the unsealed rows are reported")

	// The verifier adopts them, and the next writes seal them.
	_, err := sealer.VerifyAll(context.Background())
	require.NoError(t, err)
	assert.Empty(t, env.openFindings(t))
	w := env.send(http.MethodPost, "/public/auth/refresh", nil, withRefreshCookie(cookie.Value))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	env.sealWith(t, live, rowseal.ModeEnforce)
	access = accessTokenOf(t, w)
	require.Equal(t, http.StatusOK, env.send(http.MethodGet, "/user/ping", nil, withAuthorization(access)).Code)
}
