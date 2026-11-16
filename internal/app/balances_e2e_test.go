package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type balancesResponse struct {
	Code string `json:"code"`
	Info struct {
		Accounts []struct {
			SubAccountID *string `json:"sub_account_id"`
			Balances     []struct {
				CurrencyHK string `json:"currency_hk"`
				Available  string `json:"available"`
				Locked     string `json:"locked"`
				Total      string `json:"total"`
				Version    int    `json:"version"`
			} `json:"balances"`
		} `json:"accounts"`
		CurrencyHK string `json:"currency_hk"`
		Items      []struct {
			Kind      string `json:"kind"`
			Amount    string `json:"amount"`
			Reference *struct {
				Type string `json:"type"`
			} `json:"reference"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
	} `json:"info"`
}

// TestBalances_EndToEnd reads the balances and the history through the
// routes (#365), in enforce mode, with the OpenAPI contract checked on
// every answer: a user sees their own accounts, never another user's, and a
// balance changed in SQL is not shown.
func TestBalances_EndToEnd(t *testing.T) {
	env := newE2EEnv(t)
	sealer := rowseal.New(env.db, env.handlers.DataKeys, rowseal.ModeEnforce)
	t.Cleanup(rowseal.Use(sealer))
	t.Cleanup(audit.UseSealer(sealer))
	ctx := context.Background()
	aliceToken, alice := env.signedInUser(t, "alice@example.com")
	_, bob := env.signedInUser(t, "bob@example.com")
	var usdt, trading, bobs string
	require.NoError(t, env.db.QueryRow(`INSERT INTO currencies (status, info) VALUES ('approved',
		'{"symbol":"USDT","en_name":"Tether","fa_name":"تتر","if_fiat":false,"is_token":true,"contract":""}') RETURNING hk::text`).Scan(&usdt))
	require.NoError(t, env.db.QueryRow(`INSERT INTO sub_accounts (user_id, label) VALUES ($1, 'trading') RETURNING id::text`, alice).Scan(&trading))
	require.NoError(t, env.db.QueryRow(`INSERT INTO sub_accounts (user_id, label) VALUES ($1, 'bob') RETURNING id::text`, bob).Scan(&bobs))
	books := ledger.Ledger{DB: env.db}
	fund := func(user, sub, value, key string) {
		t.Helper()
		a := ledger.Account{UserHK: shared.UUID(mustParse(t, user))}
		if sub != "" {
			id := shared.UUID(mustParse(t, sub))
			a.SubAccountID = &id
		}
		amount := ledger.Amount{Currency: shared.UUID(mustParse(t, usdt)), Value: shared.MustNewDecimalFromString(value)}
		_, err := books.DetectDeposit(ctx, key+":detect", a, amount, ledger.Ref{Type: "deposit", ID: key})
		require.NoError(t, err)
		_, err = books.CreditDeposit(ctx, key+":credit", a, amount, ledger.Ref{Type: "deposit", ID: key})
		require.NoError(t, err)
	}
	fund(alice, "", "100", "a1")
	fund(alice, trading, "30", "a2")
	fund(bob, bobs, "999", "b1")

	get := func(path string) (int, balancesResponse) {
		t.Helper()
		status, raw := env.call(t, http.MethodGet, path, nil, aliceToken)
		var resp balancesResponse
		require.NoError(t, json.Unmarshal(raw, &resp), string(raw))
		return status, resp
	}

	status, resp := get("/api/v1/user/balances?account=all")
	require.Equal(t, http.StatusOK, status)
	require.Len(t, resp.Info.Accounts, 2)
	assert.Nil(t, resp.Info.Accounts[0].SubAccountID)
	assert.Equal(t, "100", resp.Info.Accounts[0].Balances[0].Total)
	assert.Equal(t, trading, *resp.Info.Accounts[1].SubAccountID)
	assert.Equal(t, "30", resp.Info.Accounts[1].Balances[0].Available)

	status, resp = get("/api/v1/user/balances?account=" + bobs)
	assert.Equal(t, http.StatusNotFound, status, "not Bob's sub-account")
	assert.Equal(t, "account_not_found", resp.Code)

	status, resp = get("/api/v1/user/transactions?limit=1")
	require.Equal(t, http.StatusOK, status)
	require.Len(t, resp.Info.Items, 1)
	assert.Equal(t, "deposit_credited", resp.Info.Items[0].Kind)
	assert.Equal(t, "deposit", resp.Info.Items[0].Reference.Type)
	require.NotEmpty(t, resp.Info.NextCursor)
	status, resp = get("/api/v1/user/transactions?limit=1&cursor=" + resp.Info.NextCursor)
	require.Equal(t, http.StatusOK, status)
	require.Len(t, resp.Info.Items, 1)
	assert.Equal(t, "deposit_detected", resp.Info.Items[0].Kind)
	assert.Empty(t, resp.Info.NextCursor, "the main account has two changes")

	_, err := env.db.Exec(`UPDATE balances SET available = available + 1000 WHERE user_id = $1 AND sub_account_id IS NULL`, alice)
	require.NoError(t, err)
	status, resp = get("/api/v1/user/balances")
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "balance_unverified", resp.Code)
	assert.Equal(t, usdt, resp.Info.CurrencyHK)
	var findings int
	require.NoError(t, env.db.QueryRow(`SELECT COUNT(*) FROM integrity_findings WHERE table_name = 'balances'`).Scan(&findings))
	assert.Equal(t, 1, findings)
}

func mustParse(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	require.NoError(t, err)
	return id
}
