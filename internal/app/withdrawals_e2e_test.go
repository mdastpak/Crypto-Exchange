package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/crypto/field"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"
	"github.com/mdastpak/crypto-exchange/internal/withdrawals"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserWithdrawals walks through #70 on the routes: a request starts a
// confirmation whose code goes by email, the confirmed request locks the
// amount and the fee, a retry of its key returns it, and a cancellation
// releases them.
func TestUserWithdrawals(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	const email = "withdraw@example.com"
	token, userID := env.signedInUser(t, email)
	// The destinations are encrypted with the data keys (#140).
	t.Cleanup(field.Use(field.New(env.handlers.DataKeys)))

	reg, err := assetregistry.Load(filepath.Join("..", "..", "config", "assets.yaml"))
	require.NoError(t, err)
	_, err = currencies.Store{DB: env.db}.Import(ctx, reg, audit.Event{ActorType: audit.ActorSystem}, false)
	require.NoError(t, err)
	var usdt string
	require.NoError(t, env.db.QueryRow(`SELECT hk FROM currencies WHERE info->>'symbol' = 'USDT'`).Scan(&usdt))

	// Level 2, which crypto withdrawals need, an address usable at once,
	// and 1000 USDT.
	_, err = env.db.Exec(`UPDATE users SET kyc_tier = 'STANDARD', kyc_status = 'APPROVED' WHERE hk = $1`, userID)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(ctx, env.db, rowseal.Users, "hk = $1", userID))
	levels, ok := env.handlers.User.Verification.(verification.Store)
	require.True(t, ok)
	noWait := 0
	book := destinations.Store{DB: env.db, Levels: levels,
		Settings: config.WithdrawalsConfig{MaxDestinations: 20, CryptoDestinationWaitHours: &noWait}}
	address, err := book.Add(ctx, userID, destinations.Request{Kind: destinations.KindCrypto, Network: "tron",
		Address: "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8"}, audit.Event{})
	require.NoError(t, err)
	l := ledger.Ledger{DB: env.db}
	user, err := shared.UUIDFromString(userID)
	require.NoError(t, err)
	currency, err := shared.UUIDFromString(usdt)
	require.NoError(t, err)
	deposit := ledger.Amount{Currency: currency, Value: shared.NewDecimalFromInt64(1000)}
	_, err = l.DetectDeposit(ctx, "e2e-deposit:detect", ledger.Account{UserHK: user}, deposit, ledger.Ref{Type: "deposit", ID: "e2e"})
	require.NoError(t, err)
	_, err = l.CreditDeposit(ctx, "e2e-deposit:credit", ledger.Account{UserHK: user}, deposit, ledger.Ref{Type: "deposit", ID: "e2e"})
	require.NoError(t, err)

	request := map[string]any{"currency_hk": usdt, "network": "tron", "destination_id": address.ID, "amount": "100"}
	send := func(body map[string]any) (int, []byte) {
		t.Helper()
		payload, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/user/withdrawals", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", "e2e-withdrawal-1")
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)
		return w.Code, w.Body.Bytes()
	}
	status, raw := send(request)
	require.Equal(t, http.StatusAccepted, status, string(raw))
	var started struct {
		Info struct {
			Confirmation struct {
				ID     string `json:"id"`
				Method string `json:"method"`
			} `json:"confirmation"`
		} `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &started))
	assert.Equal(t, "email", started.Info.Confirmation.Method)
	code := env.sender.last(email)
	require.NotEmpty(t, code)

	request["confirmation_id"] = started.Info.Confirmation.ID
	status, raw = send(request)
	assert.Equal(t, http.StatusConflict, status, "not confirmed yet")
	assert.Contains(t, string(raw), "confirmation_not_confirmed")
	status, resp := env.post(t, "/api/v1/user/confirmations/"+started.Info.Confirmation.ID, map[string]string{"code": code}, token)
	require.Equal(t, http.StatusOK, status, resp.Message)

	status, raw = send(request)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var created struct {
		Info withdrawals.Withdrawal `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &created))
	w := created.Info
	assert.Equal(t, withdrawals.StatusQueued, w.Status)
	assert.Equal(t, "101", w.Total.String())
	b, err := l.Balance(ctx, ledger.Account{UserHK: user}, currency)
	require.NoError(t, err)
	assert.Equal(t, "899", b.Available.String())

	status, raw = send(request)
	assert.Equal(t, http.StatusOK, status, "a retry of the key")
	assert.Contains(t, string(raw), w.ID)

	status, raw = env.call(t, http.MethodGet, "/api/v1/user/withdrawals", nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), w.ID)

	status, raw = env.call(t, http.MethodPost, "/api/v1/user/withdrawals/"+w.ID+"/cancel", nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	b, err = l.Balance(ctx, ledger.Account{UserHK: user}, currency)
	require.NoError(t, err)
	assert.Equal(t, "1000", b.Available.String(), "released")

	assert.Len(t, auditEvents(t, env.db, audit.ActionUserWithdrawalRequest), 1)
	assert.Len(t, auditEvents(t, env.db, audit.ActionUserWithdrawalCancel), 1)
}
