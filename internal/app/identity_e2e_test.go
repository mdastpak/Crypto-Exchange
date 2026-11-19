package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/idcheck"
	"github.com/mdastpak/crypto-exchange/internal/identity"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type identityResponse struct {
	Code string         `json:"code"`
	Info idcheck.Status `json:"info"`
}

// TestUserIdentity_EndToEnd verifies a user's identity through the routes
// (#158), in enforce mode, with the OpenAPI contract checked on every
// answer: a mismatch, then a match; one person, one account; a provider
// that cannot answer; and level 1 once a card of the user matched.
func TestUserIdentity_EndToEnd(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	require.NotNil(t, env.handlers.DataKeys)
	sealer := rowseal.New(env.db, env.handlers.DataKeys, rowseal.ModeEnforce)
	t.Cleanup(rowseal.Use(sealer))
	t.Cleanup(audit.UseSealer(sealer))
	t.Cleanup(field.Use(field.New(env.handlers.DataKeys)))
	const nationalID, card = "0012345679", "6037990000000014"
	stub := &identity.Stub{Births: map[string]string{nationalID: "1991-08-16"}, Owners: map[string]string{card: nationalID}}
	env.handlers.User.Identity = idcheck.Service{DB: env.db, Inquiry: stub, Sealer: env.handlers.DataKeys}
	store := env.handlers.User.Destinations.(destinations.Store)
	store.Ownership, store.NationalIDs = stub, env.handlers.DataKeys
	env.handlers.User.Destinations = store

	token, userID := env.signedInUser(t, "identity@example.com")
	other, _ := env.signedInUser(t, "identity2@example.com")
	call := func(method string, body any, tok string) (int, identityResponse) {
		t.Helper()
		status, raw := env.call(t, method, "/api/v1/user/identity", body, tok)
		var resp identityResponse
		require.NoError(t, json.Unmarshal(raw, &resp), string(raw))
		return status, resp
	}

	status, resp := call(http.MethodGet, nil, token)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, idcheck.StateNone, resp.Info.State)

	status, resp = call(http.MethodPost, map[string]string{"national_id": "12", "birth_date": "1991-08-16"}, token)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "validation_failed", resp.Code)

	status, resp = call(http.MethodPost, map[string]string{"national_id": nationalID, "birth_date": "1991-08-17"}, token)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, idcheck.StateRejected, resp.Info.State)
	assert.Equal(t, "mismatched", resp.Info.Reason)

	status, resp = call(http.MethodPost, map[string]string{"national_id": nationalID, "birth_date": "1991-08-16"}, token)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, idcheck.StateVerified, resp.Info.State)
	assert.Equal(t, "00******79", resp.Info.NationalID)

	status, resp = call(http.MethodPost, map[string]string{"national_id": nationalID, "birth_date": "1991-08-16"}, other)
	assert.Equal(t, http.StatusConflict, status, "one person, one account")
	assert.Equal(t, "national_id_taken", resp.Code)

	stub.SetUnavailable(true)
	stub.Births["0499370899"] = "1985-01-02"
	status, resp = call(http.MethodPost, map[string]string{"national_id": "0499370899", "birth_date": "1985-01-02"}, other)
	assert.Equal(t, http.StatusAccepted, status, "pending while the provider is down")
	assert.Equal(t, idcheck.StatePending, resp.Info.State)
	stub.SetUnavailable(false)

	// A card whose owner matched, with the verified identity: level 1.
	_, err := store.Add(ctx, userID, destinations.Request{Kind: destinations.KindCard, Number: card}, audit.Event{})
	require.NoError(t, err)
	status, raw := env.call(t, http.MethodGet, "/api/v1/user/verification", nil, token)
	require.Equal(t, http.StatusOK, status)
	var verification struct {
		Info struct {
			Level int `json:"level"`
		} `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &verification))
	assert.Equal(t, 1, verification.Info.Level)
}
