package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/auth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// call sends a request with a JSON body, which may be nil, and returns the
// status and the raw response body.
func (e *e2eEnv) call(t *testing.T, method, path string, body interface{}, token string) (int, []byte) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

type profileResponse struct {
	Message string `json:"message"`
	Info    struct {
		UUID        string            `json:"uuid"`
		Status      string            `json:"status"`
		Contact     map[string]string `json:"contact"`
		FirstName   *string           `json:"first_name"`
		LastName    *string           `json:"last_name"`
		DisplayName *string           `json:"display_name"`
		KYCStatus   string            `json:"kyc_status"`
		KYCTier     string            `json:"kyc_tier"`
		CreatedAt   string            `json:"created_at"`
	} `json:"info"`
}

func (e *e2eEnv) profile(t *testing.T, method string, body interface{}, token string) (int, profileResponse) {
	t.Helper()
	status, raw := e.call(t, method, "/api/v1/user/profile", body, token)
	var resp profileResponse
	require.NoError(t, json.Unmarshal(raw, &resp), string(raw))
	return status, resp
}

// signedInUser signs a user in and returns the access token and the user ID.
func (e *e2eEnv) signedInUser(t *testing.T, contact string) (string, string) {
	t.Helper()
	status, resp := e.signInUser(t, contact)
	require.Equal(t, http.StatusOK, status, resp.Message)
	token := resp.Info["access_token"].(string)
	claims, err := auth.ParseToken(token, auth.RoleUser, auth.TokenTypeAccess)
	require.NoError(t, err)
	return token, claims.UUID
}

// TestUserProfile_EndToEnd signs two users in and checks that each one reads
// and changes only their own profile, and that account changes in the
// database take effect on tokens that were already issued.
func TestUserProfile_EndToEnd(t *testing.T) {
	env := newE2EEnv(t)
	aliceToken, aliceID := env.signedInUser(t, "alice@example.com")
	bobToken, bobID := env.signedInUser(t, "09121234567")

	t.Run("a user reads their own profile with masked contacts", func(t *testing.T) {
		status, raw := env.call(t, http.MethodGet, "/api/v1/user/profile", nil, aliceToken)
		require.Equal(t, http.StatusOK, status, string(raw))
		assert.NotContains(t, string(raw), "alice@example.com", "the contact is masked")

		status, resp := env.profile(t, http.MethodGet, nil, aliceToken)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, aliceID, resp.Info.UUID)
		assert.Equal(t, "ACTIVE", resp.Info.Status)
		assert.Equal(t, map[string]string{"email": "ali*****@example.com"}, resp.Info.Contact)
		assert.Nil(t, resp.Info.FirstName)

		// A second address, such as one waiting to replace the first, is not
		// what the user signs in with.
		_, err := env.db.Exec(`INSERT INTO user_contacts (user_hk, type, value, is_primary) VALUES ($1, 'email', 'zzz.pending@example.com', FALSE)`, aliceID)
		require.NoError(t, err)
		_, resp = env.profile(t, http.MethodGet, nil, aliceToken)
		assert.Equal(t, map[string]string{"email": "ali*****@example.com"}, resp.Info.Contact, "only primary contacts are shown")
		_, err = env.db.Exec(`DELETE FROM user_contacts WHERE value = 'zzz.pending@example.com'`)
		require.NoError(t, err)
		assert.Equal(t, "PENDING", resp.Info.KYCStatus)
		assert.Equal(t, "NONE", resp.Info.KYCTier)
		created, err := time.Parse(time.RFC3339, resp.Info.CreatedAt)
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now(), created, time.Minute, "created_at is the instant of the first sign-in")

		status, resp = env.profile(t, http.MethodGet, nil, bobToken)
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, bobID, resp.Info.UUID, "each token reads its own profile")
		assert.Equal(t, map[string]string{"mobile": "091*****567"}, resp.Info.Contact)
	})

	t.Run("a user changes only their own profile", func(t *testing.T) {
		status, resp := env.profile(t, http.MethodPatch, map[string]string{"first_name": " Alice ", "display_name": "alice"}, aliceToken)
		require.Equal(t, http.StatusOK, status, resp.Message)
		require.NotNil(t, resp.Info.FirstName)
		assert.Equal(t, "Alice", *resp.Info.FirstName)
		assert.Equal(t, aliceID, resp.Info.UUID)

		// A body cannot name another user.
		status, raw := env.call(t, http.MethodPatch, "/api/v1/user/profile", map[string]string{"uuid": bobID, "first_name": "Mallory"}, aliceToken)
		assert.Equal(t, http.StatusBadRequest, status, string(raw))
		assert.Contains(t, string(raw), `"uuid":"is not accepted"`)

		status, resp = env.profile(t, http.MethodGet, nil, bobToken)
		require.Equal(t, http.StatusOK, status)
		assert.Nil(t, resp.Info.FirstName, "another user's profile is unchanged")
		assert.Nil(t, resp.Info.DisplayName)

		var firstName, displayName string
		require.NoError(t, env.db.QueryRow(`SELECT first_name, display_name FROM users WHERE hk = $1`, aliceID).Scan(&firstName, &displayName))
		assert.Equal(t, "Alice", firstName, "the change is persisted")
		assert.Equal(t, "alice", displayName)
	})

	t.Run("an update changes only the fields it sends", func(t *testing.T) {
		status, resp := env.profile(t, http.MethodPatch, map[string]string{"display_name": ""}, aliceToken)
		require.Equal(t, http.StatusOK, status, resp.Message)
		assert.Nil(t, resp.Info.DisplayName, "an empty name clears the field")
		require.NotNil(t, resp.Info.FirstName)
		assert.Equal(t, "Alice", *resp.Info.FirstName)
	})

	t.Run("each change is audited with what changed", func(t *testing.T) {
		// An update that changes nothing records nothing.
		status, resp := env.profile(t, http.MethodPatch, map[string]string{"first_name": "Alice"}, aliceToken)
		require.Equal(t, http.StatusOK, status, resp.Message)

		rows, err := env.db.Query(`SELECT actor_type, actor_id, target_id, changes::text FROM audit_events
			WHERE action = 'user.profile_update' ORDER BY hk`)
		require.NoError(t, err)
		defer rows.Close()
		var changes []string
		for rows.Next() {
			var actor, actorID, target, change string
			require.NoError(t, rows.Scan(&actor, &actorID, &target, &change))
			assert.Equal(t, "user", actor)
			assert.Equal(t, aliceID, actorID)
			assert.Equal(t, aliceID, target)
			changes = append(changes, change)
		}
		assert.Equal(t, []string{
			`{"after": {"first_name": "Alice", "display_name": "alice"}, "before": {"first_name": null, "display_name": null}}`,
			`{"after": {"display_name": null}, "before": {"display_name": "alice"}}`,
		}, changes)
	})

	t.Run("invalid updates are rejected", func(t *testing.T) {
		for _, body := range []interface{}{
			map[string]string{"first_name": "a\u202Eb"},
			map[string]string{"status": "ACTIVE"},
			map[string]string{"kyc_tier": "INSTITUTIONAL"},
			map[string]string{},
		} {
			status, raw := env.call(t, http.MethodPatch, "/api/v1/user/profile", body, aliceToken)
			assert.Equal(t, http.StatusBadRequest, status, string(raw))
		}
		var status, tier string
		require.NoError(t, env.db.QueryRow(`SELECT status, kyc_tier FROM users WHERE hk = $1`, aliceID).Scan(&status, &tier))
		assert.Equal(t, "ACTIVE", status)
		assert.Equal(t, "NONE", tier)
	})

	t.Run("account changes take effect on issued tokens", func(t *testing.T) {
		_, err := env.db.Exec(`UPDATE users SET status = 'SUSPENDED' WHERE hk = $1`, aliceID)
		require.NoError(t, err)
		for _, method := range []string{http.MethodGet, http.MethodPatch} {
			status, resp := env.profile(t, method, map[string]string{"first_name": "Changed"}, aliceToken)
			assert.Equal(t, http.StatusForbidden, status, method)
			assert.Equal(t, "This account is not available. Please contact support.", resp.Message)
		}
		var firstName string
		require.NoError(t, env.db.QueryRow(`SELECT first_name FROM users WHERE hk = $1`, aliceID).Scan(&firstName))
		assert.Equal(t, "Alice", firstName, "a suspended account cannot change its profile")

		_, err = env.db.Exec(`UPDATE users SET status = 'ACTIVE', role = 'admin' WHERE hk = $1`, aliceID)
		require.NoError(t, err)
		status, _ := env.profile(t, http.MethodGet, nil, aliceToken)
		assert.Equal(t, http.StatusForbidden, status, "an administrator account is not served to a user token")

		// The session check of the user routes answers before the handler,
		// so a deleted user gets 403, like a suspended one (#120).
		_, err = env.db.Exec(`UPDATE users SET deleted_at = NOW() WHERE hk = $1`, bobID)
		require.NoError(t, err)
		status, resp := env.profile(t, http.MethodGet, nil, bobToken)
		assert.Equal(t, http.StatusForbidden, status)
		assert.Equal(t, "This account is not available. Please contact support.", resp.Message)
	})

	t.Run("created_at is an instant whatever the database time zone", func(t *testing.T) {
		token, id := env.signedInUser(t, "carol@example.com")
		var stored time.Time
		require.NoError(t, env.db.QueryRow(`SELECT created_at AT TIME ZONE 'UTC' FROM users WHERE hk = $1`, id).Scan(&stored))

		// In another database time zone, NOW() writes another wall-clock
		// value. The profile must still report the same instant.
		_, err := env.db.Exec(`UPDATE users SET created_at = (created_at AT TIME ZONE 'UTC') AT TIME ZONE 'Asia/Tehran' WHERE hk = $1`, id)
		require.NoError(t, err)
		_, err = env.db.Exec(`ALTER DATABASE ` + e2eDBName + ` SET timezone TO 'Asia/Tehran'`)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = env.db.Exec(`ALTER DATABASE ` + e2eDBName + ` RESET timezone`) })
		// env.db is the server's pool: new connections pick up the setting.
		env.db.SetMaxIdleConns(0)
		env.db.SetMaxIdleConns(2)

		status, resp := env.profile(t, http.MethodGet, nil, token)
		require.Equal(t, http.StatusOK, status, resp.Message)
		got, err := time.Parse(time.RFC3339, resp.Info.CreatedAt)
		require.NoError(t, err)
		assert.WithinDuration(t, stored, got, time.Second)

		_, err = env.db.Exec(`UPDATE users SET created_at = NULL WHERE hk = $1`, id)
		require.NoError(t, err)
		status, raw := env.call(t, http.MethodGet, "/api/v1/user/profile", nil, token)
		require.Equal(t, http.StatusOK, status, string(raw))
		assert.Contains(t, string(raw), `"created_at":null`)
	})

	t.Run("the old routes are gone", func(t *testing.T) {
		status, _ := env.call(t, http.MethodPost, "/api/v1/user/update", map[string]string{"first_name": "x"}, bobToken)
		assert.Equal(t, http.StatusNotFound, status)
	})
}
