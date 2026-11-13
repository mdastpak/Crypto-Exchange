package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/verification"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUserVerification_TheLevelAndItsLimits reads GET /user/verification
// (#157) for a user at level 0, and again once their tier is approved:
// what each level allows, and the rial limit of level 1 from the settings.
func TestUserVerification_TheLevelAndItsLimits(t *testing.T) {
	env := newE2EEnv(t, func(c *config.Configuration) {
		c.Verification.WithdrawalLimits = map[string]map[int]config.WithdrawalLimit{
			"IRR": {1: {Daily: "1000000000", PerTransaction: "500000000"}},
		}
	})
	token, userID := env.signedInUser(t, "levels@example.com")
	_, err := env.db.Exec(`INSERT INTO currencies (status, info) VALUES ('approved',
		'{"symbol": "IRR", "en_name": "Iranian Rial", "fa_name": "ریال", "if_fiat": true, "is_token": false, "contract": ""}')`)
	require.NoError(t, err)

	read := func() verification.Summary {
		t.Helper()
		status, raw := env.call(t, http.MethodGet, "/api/v1/user/verification", nil, token)
		require.Equal(t, http.StatusOK, status, string(raw))
		var resp struct {
			Info verification.Summary `json:"info"`
		}
		require.NoError(t, json.Unmarshal(raw, &resp), string(raw))
		return resp.Info
	}

	sum := read()
	assert.Equal(t, verification.Level0, sum.Level, "a new user has a verified contact only")
	require.Len(t, sum.Actions, len(verification.Actions))
	for _, a := range sum.Actions {
		assert.False(t, a.Allowed, "%s at level 0", a.Action)
	}
	assert.Empty(t, sum.WithdrawalLimits)

	// The tier is approved, as the KYC review (#72) will do, and sealed.
	_, err = env.db.Exec(`UPDATE users SET kyc_tier = 'BASIC', kyc_status = 'APPROVED' WHERE hk = $1`, userID)
	require.NoError(t, err)
	require.NoError(t, rowseal.Reseal(context.Background(), env.db, rowseal.Users, "hk = $1", userID))

	sum = read()
	assert.Equal(t, verification.Level1, sum.Level)
	allowed := map[verification.Action]bool{}
	for _, a := range sum.Actions {
		allowed[a.Action] = a.Allowed
	}
	assert.True(t, allowed[verification.WithdrawFiat])
	assert.False(t, allowed[verification.WithdrawCrypto], "crypto withdrawals need level 2")
	require.Len(t, sum.WithdrawalLimits, 1)
	limit := sum.WithdrawalLimits[0]
	assert.Equal(t, "IRR", limit.Symbol)
	assert.Equal(t, "1000000000", limit.Daily.String())
	assert.Equal(t, "500000000", limit.PerTransaction.String())
	assert.Equal(t, "0", limit.Used.String())
	assert.Equal(t, "1000000000", limit.Remaining.String())

	status, _ := env.call(t, http.MethodGet, "/api/v1/user/verification", nil, "")
	assert.Equal(t, http.StatusUnauthorized, status)
}
