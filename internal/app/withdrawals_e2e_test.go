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
	"github.com/mdastpak/crypto-exchange/internal/denylink"
	"github.com/mdastpak/crypto-exchange/internal/destinations"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/notify"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"
	"github.com/mdastpak/crypto-exchange/internal/withdrawals"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withdrawer is a signed-in user at level 2, with a TRON address usable at
// once and 1000 USDT.
type withdrawer struct {
	env                  *e2eEnv
	email, token, userID string
	usdt, address        string
	ledger               ledger.Ledger
}

func newWithdrawer(t *testing.T, env *e2eEnv, email string) withdrawer {
	t.Helper()
	ctx := context.Background()
	token, userID := env.signedInUser(t, email)
	// The destinations are encrypted with the data keys (#140).
	t.Cleanup(field.Use(field.New(env.handlers.DataKeys)))

	reg, err := assetregistry.Load(filepath.Join("..", "..", "config", "assets.yaml"))
	require.NoError(t, err)
	_, err = currencies.Store{DB: env.db}.Import(ctx, reg, audit.Event{ActorType: audit.ActorSystem}, false)
	require.NoError(t, err)
	var usdt string
	require.NoError(t, env.db.QueryRow(`SELECT hk FROM currencies WHERE info->>'symbol' = 'USDT'`).Scan(&usdt))

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

	w := withdrawer{env: env, email: email, token: token, userID: userID, usdt: usdt, address: address.ID,
		ledger: ledger.Ledger{DB: env.db}}
	deposit := ledger.Amount{Currency: w.uuid(t, usdt), Value: shared.NewDecimalFromInt64(1000)}
	_, err = w.ledger.DetectDeposit(ctx, "e2e-deposit:detect", w.account(t), deposit, ledger.Ref{Type: "deposit", ID: "e2e"})
	require.NoError(t, err)
	_, err = w.ledger.CreditDeposit(ctx, "e2e-deposit:credit", w.account(t), deposit, ledger.Ref{Type: "deposit", ID: "e2e"})
	require.NoError(t, err)
	return w
}

func (w withdrawer) uuid(t *testing.T, s string) shared.UUID {
	t.Helper()
	id, err := shared.UUIDFromString(s)
	require.NoError(t, err)
	return id
}

func (w withdrawer) account(t *testing.T) ledger.Account {
	return ledger.Account{UserHK: w.uuid(t, w.userID)}
}

func (w withdrawer) available(t *testing.T) string {
	t.Helper()
	b, err := w.ledger.Balance(context.Background(), w.account(t), w.uuid(t, w.usdt))
	require.NoError(t, err)
	return b.Available.String()
}

func (w withdrawer) request(amount string) map[string]any {
	return map[string]any{"currency_hk": w.usdt, "network": "tron", "destination_id": w.address, "amount": amount}
}

// send posts a withdrawal request with the idempotency key.
func (w withdrawer) send(t *testing.T, body map[string]any, key string) (int, []byte) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/withdrawals", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	w.env.router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// start sends the first step, and returns the confirmation and its code.
func (w withdrawer) start(t *testing.T, body map[string]any, key string) (id, method, code string) {
	t.Helper()
	status, raw := w.send(t, body, key)
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
	return started.Info.Confirmation.ID, started.Info.Confirmation.Method, w.env.sender.last(w.email)
}

// withdraw runs a withdrawal through both steps.
func (w withdrawer) withdraw(t *testing.T, amount, key string) withdrawals.Withdrawal {
	t.Helper()
	body := w.request(amount)
	id, _, code := w.start(t, body, key)
	status, resp := w.env.post(t, "/api/v1/user/confirmations/"+id, map[string]string{"code": code}, w.token)
	require.Equal(t, http.StatusOK, status, resp.Message)
	body["confirmation_id"] = id
	status, raw := w.send(t, body, key)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var created struct {
		Info withdrawals.Withdrawal `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &created))
	return created.Info
}

// TestUserWithdrawals walks through #70 on the routes: a request starts a
// confirmation whose code goes by email, the confirmed request locks the
// amount and the fee, a retry of its key returns it, and a cancellation
// releases them.
func TestUserWithdrawals(t *testing.T) {
	env := newE2EEnv(t)
	u := newWithdrawer(t, env, "withdraw@example.com")
	const key = "e2e-withdrawal-1"

	request := u.request("100")
	id, method, code := u.start(t, request, key)
	assert.Equal(t, "email", method)
	require.NotEmpty(t, code)

	request["confirmation_id"] = id
	status, raw := u.send(t, request, key)
	assert.Equal(t, http.StatusConflict, status, "not confirmed yet")
	assert.Contains(t, string(raw), "confirmation_not_confirmed")
	status, resp := env.post(t, "/api/v1/user/confirmations/"+id, map[string]string{"code": code}, u.token)
	require.Equal(t, http.StatusOK, status, resp.Message)

	status, raw = u.send(t, request, key)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var created struct {
		Info withdrawals.Withdrawal `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &created))
	w := created.Info
	assert.Equal(t, withdrawals.StatusQueued, w.Status)
	assert.Equal(t, "101", w.Total.String())
	assert.Equal(t, "899", u.available(t))

	status, raw = u.send(t, request, key)
	assert.Equal(t, http.StatusOK, status, "a retry of the key")
	assert.Contains(t, string(raw), w.ID)

	status, raw = env.call(t, http.MethodGet, "/api/v1/user/withdrawals", nil, u.token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), w.ID)

	status, raw = env.call(t, http.MethodPost, "/api/v1/user/withdrawals/"+w.ID+"/cancel", nil, u.token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Equal(t, "1000", u.available(t), "released")

	assert.Len(t, auditEvents(t, env.db, audit.ActionUserWithdrawalRequest), 1)
	assert.Len(t, auditEvents(t, env.db, audit.ActionUserWithdrawalCancel), 1)
}

// TestAdminPayouts walks through the payouts of #70 by the manual
// provider: a withdrawal is planned and sent, an administrator reads its
// destination and records it paid with the hash on chain, and the user
// sees it completed.
func TestAdminPayouts(t *testing.T) {
	env := newE2EEnv(t)
	ctx := context.Background()
	u := newWithdrawer(t, env, "payout@example.com")
	w := u.withdraw(t, "100", "e2e-payout-1")

	payouts, ok := env.handlers.Admin.Payouts.(withdrawals.Payouts)
	require.True(t, ok)
	_, err := payouts.Run(ctx)
	require.NoError(t, err)

	enrollment, err := env.accounts.Create(ctx, "payouts-ops@example.com")
	require.NoError(t, err)
	token := env.signInAdmin(t, enrollment)
	status, raw := env.call(t, http.MethodGet, "/api/v1/admin/payouts", nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	var list struct {
		Info []withdrawals.Payout `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &list))
	require.Len(t, list.Info, 1)
	pay := list.Info[0]
	assert.Equal(t, w.ID, pay.WithdrawalID)
	assert.Equal(t, "chain", pay.Rail)

	status, raw = env.call(t, http.MethodGet, "/api/v1/admin/payouts/"+pay.ID, nil, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	assert.Contains(t, string(raw), "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8", "the address to pay")
	assert.Len(t, auditEvents(t, env.db, audit.ActionWithdrawalPayoutView), 1)

	status, raw = env.call(t, http.MethodPost, "/api/v1/admin/payouts/"+pay.ID+"/complete", map[string]string{"tracking_id": "0x9f2c"}, token)
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = env.call(t, http.MethodPost, "/api/v1/admin/payouts/"+pay.ID+"/complete", map[string]string{"tracking_id": "0x9f2c"}, token)
	assert.Equal(t, http.StatusConflict, status, "never paid twice")
	assert.Contains(t, string(raw), "payout_not_open")

	status, raw = env.call(t, http.MethodGet, "/api/v1/user/withdrawals/"+w.ID, nil, u.token)
	require.Equal(t, http.StatusOK, status, string(raw))
	var got struct {
		Info withdrawals.Withdrawal `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, withdrawals.StatusCompleted, got.Info.Status)
	require.Len(t, got.Info.Payouts, 1)
	assert.Equal(t, "0x9f2c", got.Info.Payouts[0].TrackingID)
	assert.Empty(t, got.Info.Payouts[0].UserHK, "the user's view has no fields of the administrators")
	assert.Equal(t, "899", u.available(t), "paid: the amount and the fee left")
	assert.Len(t, auditEvents(t, env.db, audit.ActionWithdrawalPayoutComplete), 1)
}

// TestDenyLink walks through the "this was not me" link of #374 on the
// routes: the code of a withdrawal goes out with its link; opening the
// link's route changes nothing; the page's POST cancels the withdrawal,
// releases its funds, ends the user's sessions, starts the security hold,
// and emails the notice; and the link works once.
func TestDenyLink(t *testing.T) {
	env := newE2EEnv(t, func(c *config.Configuration) {
		c.Security.DenyLinks.BaseURL, c.Security.DenyLinks.TTLHours = "https://ex.example/d/", 24
	})
	u := newWithdrawer(t, env, "deny@example.com")
	w := u.withdraw(t, "100", "e2e-deny-1")
	msg, ok := env.sender.lastOf(u.email, notify.PurposeWithdrawal)
	require.True(t, ok)
	require.Len(t, msg.DenyToken, denylink.TokenLength)
	assert.Equal(t, "https://ex.example/d/"+msg.DenyToken, msg.DenyLink)
	assert.Equal(t, "899", u.available(t))

	status, _ := env.call(t, http.MethodGet, "/api/v1/public/deny/"+msg.DenyToken, nil, "")
	assert.Equal(t, http.StatusMethodNotAllowed, status, "opening the link changes nothing")
	assert.Equal(t, "899", u.available(t))

	status, raw := env.call(t, http.MethodPost, "/api/v1/public/deny/"+msg.DenyToken, nil, "")
	require.Equal(t, http.StatusOK, status, string(raw))
	var denied struct {
		Info denylink.Outcome `json:"info"`
	}
	require.NoError(t, json.Unmarshal(raw, &denied))
	assert.Equal(t, 1, denied.Info.WithdrawalsCancelled)
	assert.Equal(t, 1, denied.Info.SessionsEnded)
	assert.NotContains(t, string(raw), u.email, "the answer names no account")
	assert.Equal(t, "1000", u.available(t), "released")

	status, _ = env.call(t, http.MethodGet, "/api/v1/user/withdrawals/"+w.ID, nil, u.token)
	assert.Equal(t, http.StatusUnauthorized, status, "the session ended")
	notice, ok := env.sender.lastOf(u.email, notify.PurposeDenied)
	require.True(t, ok, "the user is told by email")
	assert.Equal(t, 1, notice.Cancelled)
	assert.Equal(t, 24, notice.HoldHours)
	assert.Len(t, auditEvents(t, env.db, audit.ActionUserOperationDeny), 1)

	status, raw = env.call(t, http.MethodPost, "/api/v1/public/deny/"+msg.DenyToken, nil, "")
	assert.Equal(t, http.StatusNotFound, status, "once")
	assert.Contains(t, string(raw), "deny_link_invalid")
}
