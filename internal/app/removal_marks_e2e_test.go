package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The removal marks of #248: the server, as the runtime role, which cannot
// delete these rows, marks them, and every reader treats a marked row as
// gone, while its value can be taken again.

// TestRemovalMarks_ARemovedContactIsGone: a contact marked removed is not
// listed, is not in the profile, does not sign in to its account, and can
// be taken by another account.
func TestRemovalMarks_ARemovedContactIsGone(t *testing.T) {
	env := newE2EEnv(t)
	const email, mobile = "marks@example.com", "09121110077"
	token, _ := env.signedIn(t, email)
	userID := userOf(t, token)
	id, accountCode, newCode := env.startChange(t, token, map[string]string{"action": "add", "value": mobile}, email, mobile)
	status, resp := env.confirmChange(t, token, id, accountCode, newCode)
	require.Equal(t, http.StatusOK, status, resp.Message)
	require.Equal(t, []string{"email", "mobile"}, env.contactTypes(t, token))

	id, accountCode, _ = env.startChange(t, token, map[string]string{"action": "remove", "type": "mobile"}, mobile, "")
	status, resp = env.confirmChange(t, token, id, accountCode, "")
	require.Equal(t, http.StatusOK, status, resp.Message)
	var marked int
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM user_contacts
		WHERE user_hk = $1 AND type = 'mobile' AND removed_at IS NOT NULL`, userID).Scan(&marked))
	require.Equal(t, 1, marked, "the removed contact is marked, not deleted")

	assert.Equal(t, []string{"email"}, env.contactTypes(t, token))
	status, profile := env.profile(t, http.MethodGet, nil, token)
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, profile.Info.Contact, "mobile")

	other, _ := env.signedIn(t, mobile)
	assert.NotEqual(t, userID, userOf(t, other), "a removed contact does not sign in to its account; it creates another")
	again, _ := env.signedIn(t, mobile)
	assert.Equal(t, userOf(t, other), userOf(t, again), "and then signs in to that one")

	// The first user can add the type again, with another number.
	id, accountCode, newCode = env.startChange(t, token, map[string]string{"action": "add", "value": "09121110078"}, email, "09121110078")
	status, resp = env.confirmChange(t, token, id, accountCode, newCode)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Equal(t, []string{"email", "mobile"}, env.contactTypes(t, token))
	var rows int
	require.NoError(t, env.db.QueryRow(`SELECT count(*) FROM user_contacts WHERE user_hk = $1 AND type = 'mobile'`, userID).Scan(&rows))
	assert.Equal(t, 2, rows, "the removed contact stays for the record")
}

// TestRemovalMarks_AReleasedSymbolIsFree: a previous symbol marked
// released is no longer one of the currency's, and another currency can
// take it.
func TestRemovalMarks_AReleasedSymbolIsFree(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	enrollment, err := env.accounts.Create(ctx, "marks-admin@example.com")
	require.NoError(t, err)
	admin := env.signInAdmin(t, enrollment)
	create := func(symbol, contract string) (int, string) {
		t.Helper()
		c := testCurrency(symbol)
		c["info"].(map[string]interface{})["contract"] = contract
		status, raw := env.call(t, http.MethodPost, "/api/v1/admin/currencies", c, admin)
		var resp struct {
			Info struct {
				HK string `json:"hk"`
			} `json:"info"`
		}
		_ = json.Unmarshal(raw, &resp)
		return status, resp.Info.HK
	}
	status, old := create("OMG", "contract-omg")
	require.Equal(t, http.StatusCreated, status)
	status, raw := env.call(t, http.MethodPut, "/api/v1/admin/currencies/"+old, map[string]interface{}{"info": map[string]string{"symbol": "OMGX"}}, admin)
	require.Equal(t, http.StatusOK, status, string(raw))
	status, _ = create("OMG", "contract-omg-2")
	require.Equal(t, http.StatusConflict, status, "a previous symbol is reserved")

	status, raw = env.call(t, http.MethodDelete, "/api/v1/admin/currencies/"+old+"/symbols/OMG", nil, admin)
	require.Equal(t, http.StatusOK, status, string(raw))
	var releasedBy string
	require.NoError(t, env.db.QueryRow(`SELECT released_by FROM currency_symbols
		WHERE currency_hk = $1 AND symbol = 'OMG' AND released_at IS NOT NULL`, old).Scan(&releasedBy))
	assert.NotEmpty(t, releasedBy, "the released symbol is marked with who released it")

	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/public/currencies?symbol=OMGX", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var listed struct {
		Info struct {
			Items []models.Currency `json:"items"`
		} `json:"info"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &listed))
	require.Len(t, listed.Info.Items, 1)
	assert.Empty(t, listed.Info.Items[0].PreviousSymbols, "a released symbol is not a previous symbol")
	status, _ = env.call(t, http.MethodDelete, "/api/v1/admin/currencies/"+old+"/symbols/OMG", nil, admin)
	assert.Equal(t, http.StatusNotFound, status, "a released symbol cannot be released again")

	status, taken := create("OMG", "contract-omg-3")
	require.Equal(t, http.StatusCreated, status, "another currency takes a released symbol")
	status, _ = env.call(t, http.MethodPut, "/api/v1/admin/currencies/"+old, map[string]interface{}{"info": map[string]string{"symbol": "OMG"}}, admin)
	assert.Equal(t, http.StatusConflict, status, "and holds it")
	var held string
	require.NoError(t, env.db.QueryRow(`SELECT currency_hk FROM currency_symbols WHERE symbol = 'OMG' AND released_at IS NULL`).Scan(&held))
	assert.Equal(t, taken, held)
}
