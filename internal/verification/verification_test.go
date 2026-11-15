package verification

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/domain/shared"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dec(s string) shared.Decimal { return shared.MustNewDecimalFromString(s) }

// defaultPolicy is the policy of the default settings.
func defaultPolicy(t *testing.T) Policy {
	t.Helper()
	p, err := NewPolicy(config.VerificationConfig{
		MinLevel:         config.DefaultVerificationMinLevel(),
		WithdrawalLimits: config.DefaultWithdrawalLimits(),
	})
	require.NoError(t, err)
	return p
}

func TestLevelOf(t *testing.T) {
	for _, tc := range []struct {
		tier   shared.KYCTier
		status shared.KYCStatus
		want   Level
	}{
		{shared.KYCTierNone, shared.KYCStatusApproved, Level0},
		{shared.KYCTierNone, shared.KYCStatusPending, Level0},
		{shared.KYCTierBasic, shared.KYCStatusApproved, Level1},
		{shared.KYCTierStandard, shared.KYCStatusApproved, Level2},
		{shared.KYCTierEnhanced, shared.KYCStatusApproved, Level2},
		{shared.KYCTierInstitutional, shared.KYCStatusApproved, Level2},
		// Only an approved tier counts.
		{shared.KYCTierBasic, shared.KYCStatusPending, Level0},
		{shared.KYCTierBasic, shared.KYCStatusUnderReview, Level0},
		{shared.KYCTierStandard, shared.KYCStatusExpired, Level0},
		{shared.KYCTierStandard, shared.KYCStatusRejected, Level0},
		{shared.KYCTier("PLATINUM"), shared.KYCStatusApproved, Level0},
	} {
		assert.Equal(t, tc.want, LevelOf(tc.tier, tc.status), "%s %s", tc.tier, tc.status)
	}
}

// TestPolicy_EveryActionAtEveryLevel is the table of the product policy
// with the default settings.
func TestPolicy_EveryActionAtEveryLevel(t *testing.T) {
	p := defaultPolicy(t)
	allowed := map[Level][]Action{
		Level0: nil,
		Level1: {DepositFiat, DepositCrypto, TradeOTC, TradeMarket, WithdrawFiat},
		Level2: Actions,
	}
	for level, actions := range allowed {
		for _, a := range Actions {
			assert.Equal(t, slices.Contains(actions, a), p.Allows(level, a), "level %d, %s", level, a)
		}
	}
	assert.False(t, p.Allows(Level2, Action("fly")), "an unknown action is never allowed")
	assert.Equal(t, Level2, p.Required(WithdrawCrypto))

	// The rial's limits: daily at level 1, none at level 2.
	limit := func(level Level, symbols ...string) Limit {
		t.Helper()
		l, ok := p.Limit(level, symbols...)
		require.True(t, ok, "level %d, %v", level, symbols)
		return l
	}
	assert.Equal(t, "1000000000", limit(Level1, "IRR").Daily.String())
	assert.Equal(t, Limit{}, limit(Level2, "IRR"), "level 2 has no limit of the exchange")
	assert.Nil(t, limit(Level1, "IRR").PerTransaction)
	assert.Equal(t, Limit{}, limit(Level2, "USDT"), "level 2 limits no currency, so none is refused there")
	_, ok := p.Limit(Level1, "USDT")
	assert.False(t, ok, "level 1 limits the rial, so a currency without a limit is not unlimited there (#406)")
}

func TestPolicy_TheSettingsChangeIt(t *testing.T) {
	p, err := NewPolicy(config.VerificationConfig{
		MinLevel: map[string]int{config.ActionWithdrawCrypto: 1, config.ActionTradeMarket: 0},
		WithdrawalLimits: map[string]map[int]config.WithdrawalLimit{
			"USDT": {1: {Daily: "1000", PerTransaction: "300"}},
		},
	})
	require.NoError(t, err)
	assert.True(t, p.Allows(Level1, WithdrawCrypto))
	assert.True(t, p.Allows(Level0, TradeMarket))
	assert.False(t, p.Allows(Level0, TradeOTC), "an action the settings leave out takes its default")
	usdt, ok := p.Limit(Level1, "USDT")
	require.True(t, ok)
	assert.Equal(t, "300", usdt.PerTransaction.String())
	_, ok = p.Limit(Level1, "IRR")
	assert.False(t, ok, "the settings replace the default limits, and the rial has none at a limited level")

	_, err = NewPolicy(config.VerificationConfig{MinLevel: map[string]int{config.ActionDepositFiat: 3}})
	require.Error(t, err)
	_, err = NewPolicy(config.VerificationConfig{WithdrawalLimits: map[string]map[int]config.WithdrawalLimit{"IRR": {1: {Daily: "-1"}}}})
	require.Error(t, err)
}

func TestPolicy_CheckWithdrawal(t *testing.T) {
	p, err := NewPolicy(config.VerificationConfig{
		WithdrawalLimits: map[string]map[int]config.WithdrawalLimit{
			"IRR": {1: {Daily: "1000", PerTransaction: "600"}, 2: {Daily: "5000"}},
		},
	})
	require.NoError(t, err)
	const irr = "c0ffee00-0000-4000-8000-000000000001"
	refusal := func(err error) *Refusal {
		t.Helper()
		var r *Refusal
		require.ErrorAs(t, err, &r)
		return r
	}

	require.NoError(t, p.CheckWithdrawal(Level1, true, []string{"IRR"}, irr, dec("600"), dec("400")), "up to the limit exactly")

	r := refusal(p.CheckWithdrawal(Level0, true, []string{"IRR"}, irr, dec("1"), dec("0")))
	assert.Equal(t, CodeVerificationRequired, r.Code)
	assert.Equal(t, WithdrawFiat, r.Action)
	assert.Equal(t, Level1, r.Required)

	r = refusal(p.CheckWithdrawal(Level1, false, []string{"USDT"}, irr, dec("1"), dec("0")))
	assert.Equal(t, CodeVerificationRequired, r.Code, "crypto needs level 2")
	assert.Equal(t, Level2, r.Required)

	r = refusal(p.CheckWithdrawal(Level1, true, []string{"IRR"}, irr, dec("601"), dec("0")))
	assert.Equal(t, CodeWithdrawalLimitExceeded, r.Code)
	assert.Equal(t, PeriodTransaction, r.Period)
	assert.Equal(t, "600", r.Limit.String())

	r = refusal(p.CheckWithdrawal(Level1, true, []string{"IRR"}, irr, dec("500"), dec("700")))
	assert.Equal(t, PeriodDaily, r.Period)
	assert.Equal(t, "1000", r.Limit.String())
	assert.Equal(t, "300", r.Remaining().String())

	r = refusal(p.CheckWithdrawal(Level1, true, []string{"IRR"}, irr, dec("1"), dec("1200")))
	assert.Equal(t, "0", r.Remaining().String(), "never below zero")

	require.NoError(t, p.CheckWithdrawal(Level2, true, []string{"IRR"}, irr, dec("4000"), dec("1000")), "level 2 has its own limit")

	// A currency without a limit at a level that limits currencies is
	// refused, not unlimited (#406).
	r = refusal(p.CheckWithdrawal(Level2, false, []string{"BTC"}, irr, dec("1"), dec("0")))
	assert.Equal(t, CodeWithdrawalLimitUnconfigured, r.Code)
	assert.Equal(t, irr, r.CurrencyHK)
}

// TestPolicy_TheLimitFollowsTheCurrency checks #406: the limit of a
// currency follows it through a rename, by its previous symbol, the
// current symbol's entry first; a new or migrated currency takes the
// default of the level, and without one it is refused.
func TestPolicy_TheLimitFollowsTheCurrency(t *testing.T) {
	const irt = "c0ffee00-0000-4000-8000-000000000002"
	p, err := NewPolicy(config.VerificationConfig{
		MinLevel: map[string]int{config.ActionWithdrawCrypto: 1},
		WithdrawalLimits: map[string]map[int]config.WithdrawalLimit{
			"IRR":  {1: {Daily: "1000"}},
			"USDT": {1: {Daily: "50"}},
			"TUSD": {1: {Daily: "70"}},
		},
	})
	require.NoError(t, err)

	// IRR renamed to IRT: its symbols are IRT, then IRR.
	r := new(Refusal)
	require.ErrorAs(t, p.CheckWithdrawal(Level1, true, []string{"IRT", "IRR"}, irt, dec("1001"), dec("0")), &r)
	assert.Equal(t, CodeWithdrawalLimitExceeded, r.Code, "the limit of IRR still applies to IRT")
	assert.Equal(t, "1000", r.Limit.String())
	require.NoError(t, p.CheckWithdrawal(Level1, true, []string{"IRT", "IRR"}, irt, dec("1000"), dec("0")))

	// The entry of the current symbol comes first.
	l, ok := p.Limit(Level1, "TUSD", "USDT")
	require.True(t, ok)
	assert.Equal(t, "70", l.Daily.String())

	// A new currency, or the target of a migration, with a symbol of its own.
	require.ErrorAs(t, p.CheckWithdrawal(Level1, false, []string{"NEWCOIN"}, irt, dec("1"), dec("0")), &r)
	assert.Equal(t, CodeWithdrawalLimitUnconfigured, r.Code)

	// With a default of the level, it takes that.
	p, err = NewPolicy(config.VerificationConfig{
		MinLevel: map[string]int{config.ActionWithdrawCrypto: 1},
		WithdrawalLimits: map[string]map[int]config.WithdrawalLimit{
			"IRR":              {1: {Daily: "1000"}},
			config.AnyCurrency: {1: {PerTransaction: "5"}},
		},
	})
	require.NoError(t, err)
	require.ErrorAs(t, p.CheckWithdrawal(Level1, false, []string{"NEWCOIN"}, irt, dec("6"), dec("0")), &r)
	assert.Equal(t, CodeWithdrawalLimitExceeded, r.Code)
	assert.Equal(t, "5", r.Limit.String())
	require.NoError(t, p.CheckWithdrawal(Level1, false, []string{"NEWCOIN"}, irt, dec("5"), dec("0")))
	assert.NotContains(t, p.symbols(), config.AnyCurrency, "the default is no currency of the summary")
}

func TestSend_AnswersTheRefusals(t *testing.T) {
	gin.SetMode(gin.TestMode)
	send := func(err error) (int, map[string]any) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		require.True(t, Send(c, err))
		var body map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		return w.Code, body
	}

	status, body := send(&Refusal{Code: CodeVerificationRequired, Action: WithdrawCrypto, Level: Level1, Required: Level2})
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, CodeVerificationRequired, body["code"])
	assert.Equal(t, map[string]any{"action": "withdraw_crypto", "level": float64(1), "required_level": float64(2)}, body["info"])

	status, body = send(&Refusal{Code: CodeWithdrawalLimitExceeded, Action: WithdrawFiat, Level: Level1,
		CurrencyHK: "hk", Period: PeriodDaily, Limit: dec("1000"), Used: dec("800")})
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, CodeWithdrawalLimitExceeded, body["code"])
	assert.Equal(t, map[string]any{"action": "withdraw_fiat", "level": float64(1), "currency_hk": "hk", "period": "24h",
		"limit": "1000", "used": "800", "remaining": "200"}, body["info"])

	status, body = send(&Refusal{Code: CodeWithdrawalLimitUnconfigured, Action: WithdrawCrypto, Level: Level2, CurrencyHK: "hk"})
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, CodeWithdrawalLimitUnconfigured, body["code"])
	assert.Equal(t, map[string]any{"action": "withdraw_crypto", "level": float64(2), "currency_hk": "hk"}, body["info"],
		"no limit to show, and no remaining amount")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	assert.False(t, Send(c, assert.AnError), "another error is not answered")
}

// TestActions_AreTheSettingsActions keeps the actions and the keys of
// verification.min_level the same.
func TestActions_AreTheSettingsActions(t *testing.T) {
	names := make([]string, 0, len(Actions))
	for _, a := range Actions {
		names = append(names, string(a))
	}
	assert.Equal(t, config.VerificationActions, names)
}
