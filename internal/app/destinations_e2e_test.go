package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// destinationResponse is a response of the withdrawal destination routes.
type destinationResponse struct {
	Code string          `json:"code"`
	Info json.RawMessage `json:"info"`
}

func (e *e2eEnv) destinationCall(t *testing.T, method, path string, body interface{}, token string) (int, destinationResponse) {
	t.Helper()
	status, raw := e.call(t, method, path, body, token)
	var resp destinationResponse
	require.NoError(t, json.Unmarshal(raw, &resp), string(raw))
	return status, resp
}

// TestUserDestinations_TheBookOfAUser walks through #160: the guards of an
// addition in their order, a crypto address that needs level 2 and waits a
// day, a card that must be the user's own, and a removal.
func TestUserDestinations_TheBookOfAUser(t *testing.T) {
	// The server sets the provider in configureDestinations, which Run
	// calls; this test sets a stub on the store, below.
	env := newE2EEnv(t)
	ctx := context.Background()
	// The server encrypts the fields with its data keys once it runs
	// (useFieldEncryption); the environment builds the handlers only.
	require.NotNil(t, env.handlers.DataKeys)
	t.Cleanup(field.Use(field.New(env.handlers.DataKeys)))
	token, userID := env.signedInUser(t, "book@example.com")
	const path = "/api/v1/user/withdrawal-destinations"
	tron := map[string]string{"kind": "crypto", "network": "tron", "address": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "label": "cold wallet"}
	refused := func(code string) {
		t.Helper()
		status, resp := env.destinationCall(t, http.MethodPost, path, tron, token)
		assert.Equal(t, http.StatusForbidden, status)
		assert.Equal(t, code, resp.Code)
	}

	status, resp := env.destinationCall(t, http.MethodGet, path, nil, token)
	require.Equal(t, http.StatusOK, status)
	assert.JSONEq(t, `{"items": []}`, string(resp.Info))

	// A destination is for funds: a second factor, proved in the session.
	refused("second_factor_required")
	env.setPassword(t, token, testPassword)
	refused("step_up_required")
	status, stepped := env.post(t, stepUpRoute, map[string]string{"password": testPassword}, token)
	require.Equal(t, http.StatusOK, status, stepped.Message)

	// Then the level of the withdrawals it is for: 2 for crypto.
	refused("verification_required")
	_, err := env.db.Exec(`UPDATE users SET kyc_tier = 'STANDARD', kyc_status = 'APPROVED' WHERE hk = $1`, userID)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, env.db, rowseal.Users, "hk = $1", userID))

	// And the new password holds new destinations for a day (#251).
	refused("security_hold")
	_, err = env.db.Exec(`UPDATE users SET security_hold_until = NULL WHERE hk = $1`, userID)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, env.db, rowseal.Users, "hk = $1", userID))

	status, resp = env.destinationCall(t, http.MethodPost, path, tron, token)
	require.Equal(t, http.StatusCreated, status, string(resp.Info))
	var added destinations.Destination
	require.NoError(t, json.Unmarshal(resp.Info, &added))
	assert.Equal(t, "TR7NHq...gjLj6t", added.Masked)
	assert.Equal(t, "cold wallet", added.Label)
	assert.InDelta(t, 24*time.Hour, added.UsableFrom.Sub(added.CreatedAt), float64(time.Second))

	status, resp = env.destinationCall(t, http.MethodPost, path, tron, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "destination_exists", resp.Code)
	status, resp = env.destinationCall(t, http.MethodPost, path, map[string]string{"kind": "crypto", "network": "tron", "address": "T-not-an-address"}, token)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "destination_invalid", resp.Code)

	// A card needs the user's verified national ID, and must be theirs.
	card := map[string]string{"kind": "card", "number": "6037-9900-0000-0006"}
	status, resp = env.destinationCall(t, http.MethodPost, path, card, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "national_id_required", resp.Code)
	stub := env.withNationalID(t, userID, "0012345679")
	store, ok := env.handlers.User.Destinations.(destinations.Store)
	require.True(t, ok)
	store.Ownership, store.NationalIDs = stub, env.handlers.UserStore.Sealer
	env.handlers.User.Destinations = store
	stub.SetOwner("6037990000000006", "0499370899")
	status, resp = env.destinationCall(t, http.MethodPost, path, card, token)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "destination_not_owned", resp.Code)
	stub.SetOwner("6037990000000006", "0012345679")
	status, resp = env.destinationCall(t, http.MethodPost, path, card, token)
	require.Equal(t, http.StatusCreated, status, string(resp.Info))
	var cardAdded destinations.Destination
	require.NoError(t, json.Unmarshal(resp.Info, &cardAdded))
	assert.Equal(t, "melli", cardAdded.Bank)
	assert.Equal(t, "603799******0006", cardAdded.Masked)

	status, resp = env.destinationCall(t, http.MethodGet, path, nil, token)
	require.Equal(t, http.StatusOK, status)
	var list struct {
		Items []destinations.Destination `json:"items"`
	}
	require.NoError(t, json.Unmarshal(resp.Info, &list))
	require.Len(t, list.Items, 2)
	assert.Equal(t, cardAdded.ID, list.Items[0].ID, "the newest first")

	status, _ = env.destinationCall(t, http.MethodDelete, path+"/"+added.ID, nil, token)
	require.Equal(t, http.StatusOK, status)
	status, _ = env.destinationCall(t, http.MethodDelete, path+"/"+added.ID, nil, token)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Len(t, auditEvents(t, env.db, audit.ActionUserDestinationAdd), 3, "two added, and the card that was not the user's")
	assert.Len(t, auditEvents(t, env.db, audit.ActionUserDestinationRemove), 1)
}
