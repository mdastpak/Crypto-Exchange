package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVerification_DefaultsFillWhatTheFileLeavesOut loads a file that sets
// some of the levels and its own limits: the actions it leaves out take
// their defaults, a level of 0 is kept, and the limits replace the
// defaults instead of adding to them.
func TestVerification_DefaultsFillWhatTheFileLeavesOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	base, err := os.ReadFile(configFile("config.compose.yaml"))
	require.NoError(t, err)
	file := string(base) + `
verification:
  min_level:
    withdraw_crypto: 1
    trade_otc: 0
  withdrawal_limits:
    USDT:
      1: {daily: "1000", per_transaction: "500"}
`
	require.NoError(t, os.WriteFile(path, []byte(file), 0o600))
	require.NoError(t, loadFile(t, path, nil))

	v := Config.Verification
	assert.Equal(t, 1, v.MinLevel[ActionWithdrawCrypto])
	assert.Equal(t, 0, v.MinLevel[ActionTradeOTC], "a level of 0 is a setting, not a missing one")
	assert.Equal(t, 1, v.MinLevel[ActionWithdrawFiat], "an action the file leaves out takes its default")
	assert.Len(t, v.MinLevel, len(VerificationActions))
	assert.Equal(t, map[string]map[int]WithdrawalLimit{"USDT": {1: {Daily: "1000", PerTransaction: "500"}}}, v.WithdrawalLimits)
}

func TestVerification_DefaultsFollowThePolicy(t *testing.T) {
	levels := DefaultVerificationMinLevel()
	assert.ElementsMatch(t, VerificationActions, keysOf(levels), "every action has a default")
	assert.Equal(t, 2, levels[ActionWithdrawCrypto], "crypto withdrawals need level 2 until the policy allows level 1")
	for _, action := range []string{ActionDepositFiat, ActionDepositCrypto, ActionTradeOTC, ActionTradeMarket, ActionWithdrawFiat} {
		assert.Equal(t, 1, levels[action], action)
	}
	limits := DefaultWithdrawalLimits()
	require.Contains(t, limits, "IRR")
	assert.NotEmpty(t, limits["IRR"][1].Daily, "rial withdrawals at level 1 have a daily limit")
	assert.NotContains(t, limits["IRR"], 2, "level 2 has no limit of the exchange; the payouts follow the banks' limits")
}

func keysOf(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestVerification_ReleaseRefusesWithdrawalsAtLevel0: one wrong file must
// not let accounts that proved nothing move funds out (#440). The other
// actions stay a decision of the file, and outside release mode every
// action may be set from 0 to 2.
func TestVerification_ReleaseRefusesWithdrawalsAtLevel0(t *testing.T) {
	c := secureRelease(t)
	c.Verification.MinLevel = DefaultVerificationMinLevel()
	c.Verification.MinLevel[ActionWithdrawFiat] = 0
	c.Verification.MinLevel[ActionWithdrawCrypto] = 0
	c.Verification.MinLevel[ActionDepositCrypto] = 0
	c.Verification.MinLevel[ActionTradeOTC] = 0

	problems := problemsOf(c)
	require.Len(t, problems, 2, "the two withdrawals, not the deposit or the trade: %v", problems)
	all := strings.Join(problems, "\n")
	assert.Contains(t, all, "verification.min_level.withdraw_fiat")
	assert.Contains(t, all, "verification.min_level.withdraw_crypto")
	assert.Contains(t, all, "release mode needs level 1 or more for withdrawals")

	c.Verification.MinLevel[ActionWithdrawFiat] = 1
	c.Verification.MinLevel[ActionWithdrawCrypto] = 1
	assert.Empty(t, problemsOf(c), "level 1 is enough")

	c.Server.Mode = "debug"
	c.Verification.MinLevel[ActionWithdrawFiat] = 0
	c.Verification.MinLevel[ActionWithdrawCrypto] = 0
	assert.Empty(t, problemsOf(c), "development may allow withdrawals at level 0")
}
