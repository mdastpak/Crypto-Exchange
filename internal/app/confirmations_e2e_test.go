package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/confirm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserConfirmations_ACodeConfirmsItsWithdrawal walks through #161: the
// methods of a user, a confirmation that the withdrawal workflow (#70)
// starts, its code by email through the routes, and its consumption, once,
// by the operation it is bound to.
func TestUserConfirmations_ACodeConfirmsItsWithdrawal(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	const email = "confirm@example.com"
	token, userID := env.signedInUser(t, email)
	service, ok := env.handlers.User.Confirmations.(confirm.Service)
	require.True(t, ok)

	status, raw := env.call(t, http.MethodGet, "/api/v1/user/confirmation-method", nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	var settings struct {
		Info confirm.Settings `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &settings))
	assert.Equal(t, []confirm.Method{confirm.MethodEmail}, settings.Info.Available, "the verified email; no authenticator, no mobile")
	assert.Equal(t, confirm.MethodEmail, settings.Info.Effective)

	// Choosing a method needs a fresh proof of the second factor.
	status, resp := env.put(t, "/api/v1/user/confirmation-method", map[string]string{"method": "email"}, token)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "second_factor_required", resp.Code)
	env.setPassword(t, token, testPassword)
	status, _ = env.post(t, stepUpRoute, map[string]string{"password": testPassword}, token)
	require.Equal(t, http.StatusOK, status)
	status, resp = env.put(t, "/api/v1/user/confirmation-method", map[string]string{"method": "sms"}, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "confirmation_method_unavailable", resp.Code)
	status, resp = env.put(t, "/api/v1/user/confirmation-method", map[string]string{"method": "email"}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	assert.Len(t, auditEvents(t, env.db, audit.ActionUserConfirmationMethodSet), 1)

	// The withdrawal workflow starts a confirmation; the code goes by email.
	digest := confirm.Digest(confirm.PurposeWithdrawal, "100", "USDT", "destination-1")
	details := confirm.Details{Amount: "100 USDT", Destination: "TR7NHq...gjLj6t"}
	c, err := service.Start(ctx, userID, confirm.PurposeWithdrawal, digest, details, "client")
	require.NoError(t, err)
	assert.Equal(t, confirm.MethodEmail, c.Method)
	assert.Equal(t, "con*****@example.com", c.SentTo)
	code := env.sender.last(email)
	require.NotEmpty(t, code)

	path := "/api/v1/user/confirmations/" + c.ID
	status, resp = env.post(t, path, map[string]string{"code": "000000"}, token)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "confirmation_code_invalid", resp.Code)
	status, resp = env.post(t, path, map[string]string{"code": code}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.post(t, path, map[string]string{"code": code}, token)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, "confirmation_already_confirmed", resp.Code)

	// Another user cannot see it.
	other, _ := env.signedInUser(t, "other@example.com")
	status, _ = env.post(t, path, map[string]string{"code": code}, other)
	assert.Equal(t, http.StatusNotFound, status)

	// The operation consumes it once, with its own digest.
	consume := func(digest []byte) error {
		tx, err := env.db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer tx.Rollback()
		if err := service.Consume(ctx, tx, userID, c.ID, confirm.PurposeWithdrawal, digest); err != nil {
			return err
		}
		return tx.Commit()
	}
	require.ErrorIs(t, consume(confirm.Digest(confirm.PurposeWithdrawal, "100", "USDT", "destination-2")), confirm.ErrMismatch)
	require.NoError(t, consume(digest))
	require.ErrorIs(t, consume(digest), confirm.ErrConsumed)

	// A new code of another confirmation.
	second, err := service.Start(ctx, userID, confirm.PurposeWithdrawal, digest, details, "client")
	require.NoError(t, err)
	status, resp = env.post(t, "/api/v1/user/confirmations/"+second.ID+"/resend", nil, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	status, resp = env.post(t, "/api/v1/user/confirmations/"+second.ID, map[string]string{"code": env.sender.last(email)}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)

	events := auditEvents(t, env.db, audit.ActionUserOperationConfirm)
	require.Len(t, events, 3, "a wrong code and two right ones")
}

// challengeOfConfirmation returns the WebAuthn challenge of a confirmation
// by passkey, from its options.
func challengeOfConfirmation(t *testing.T, c confirm.Challenge) string {
	t.Helper()
	require.NotNil(t, c.Passkey)
	raw, err := json.Marshal(c.Passkey)
	require.NoError(t, err)
	var info map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &info))
	return challengeOfOptions(t, info)
}

// TestUserConfirmations_APasskeyConfirmsItsOwnWithdrawal confirms a
// withdrawal with a passkey (#373), through a virtual authenticator: the
// device's answer to one confirmation's challenge does not confirm another.
func TestUserConfirmations_APasskeyConfirmsItsOwnWithdrawal(t *testing.T) {
	env := newE2EEnv(t, withPasskeys)
	ctx := context.Background()
	token, userID := env.signedInUser(t, "passkey-confirm@example.com")
	service, ok := env.handlers.User.Confirmations.(confirm.Service)
	require.True(t, ok)
	env.setPassword(t, token, testPassword)
	status, _ := env.post(t, stepUpRoute, map[string]string{"password": testPassword}, token)
	require.Equal(t, http.StatusOK, status)
	device := newTestAuthenticator(t)
	env.addPasskey(t, token, device, "Laptop")

	status, raw := env.call(t, http.MethodGet, "/api/v1/user/confirmation-method", nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	var settings struct {
		Info confirm.Settings `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &settings))
	assert.Equal(t, confirm.MethodPasskey, settings.Info.Effective, "a passkey first")

	digest := confirm.Digest(confirm.PurposeWithdrawal, "50", "USDT", "destination")
	details := confirm.Details{Amount: "50 USDT", Destination: "TR7NHq...gjLj6t"}
	first, err := service.Start(ctx, userID, confirm.PurposeWithdrawal, digest, details, "client")
	require.NoError(t, err)
	second, err := service.Start(ctx, userID, confirm.PurposeWithdrawal, digest, details, "client")
	require.NoError(t, err)

	// The answer to the first challenge, sent for the second confirmation.
	answer := device.get(t, challengeOfConfirmation(t, first))
	status, resp := env.post(t, "/api/v1/user/confirmations/"+second.ID, map[string]interface{}{"passkey": answer}, token)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "confirmation_code_invalid", resp.Code)

	status, resp = env.post(t, "/api/v1/user/confirmations/"+first.ID, map[string]interface{}{"passkey": answer}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	tx, err := env.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, service.Consume(ctx, tx, userID, first.ID, confirm.PurposeWithdrawal, digest))
	require.NoError(t, tx.Commit())

	// The second needs a new ceremony, whose answer confirms it.
	status, raw = env.call(t, http.MethodPost, "/api/v1/user/confirmations/"+second.ID+"/resend", nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	var renewed struct {
		Info confirm.Challenge `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &renewed))
	status, resp = env.post(t, "/api/v1/user/confirmations/"+second.ID,
		map[string]interface{}{"passkey": device.get(t, challengeOfConfirmation(t, renewed.Info))}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)
}
