package currencies

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateCapabilities_NarrowAlongTheLifecycle(t *testing.T) {
	// Each state allows at most what the state before it allows.
	for i := 1; i < len(Lifecycles); i++ {
		before, after := StateCapabilities(Lifecycles[i-1]), StateCapabilities(Lifecycles[i])
		for _, k := range Capabilities {
			if Allows(after, k) {
				assert.True(t, Allows(before, k), "%s allows %s, which %s does not", Lifecycles[i], k, Lifecycles[i-1])
			}
		}
	}
	assert.Equal(t, models.CurrencyCapabilities{}, StateCapabilities("unknown"), "an unknown state allows nothing")
	assert.True(t, LifecycleDelistAnnounced.Reversible())
	assert.True(t, LifecycleWithdrawalOnly.Reversible())
	assert.False(t, LifecycleFrozen.Reversible())
	assert.False(t, Lifecycle("unknown").Reversible())
}

func TestLifecycleOf_AppliesTheOverrides(t *testing.T) {
	l := LifecycleOf(LifecycleFrozen, map[Capability]bool{CapabilityWithdraw: true, "unknown": true}, nil)
	assert.True(t, l.Capabilities.Withdraw)
	assert.False(t, l.Capabilities.Deposit)
	assert.True(t, Listed(models.Currency{}), "a currency without a lifecycle, from an older cache, is listed")
	assert.False(t, Listed(models.Currency{Lifecycle: LifecycleOf(LifecycleDelisted, nil, nil)}))
}

func TestSendRefusal(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	err := &Refusal{CurrencyHK: "hk", Capability: CapabilityDeposit, Lifecycle: LifecycleOf(LifecycleDepositsDisabled, nil, nil)}
	require.True(t, SendRefusal(c, err))
	assert.Equal(t, http.StatusForbidden, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, CodeCurrencyOperationDisabled, body["code"])
	assert.Contains(t, w.Body.String(), `"lifecycle":"deposits_disabled"`)
	assert.False(t, SendRefusal(c, ErrNotFound))
}

func TestMigrationCapabilities(t *testing.T) {
	active := StateCapabilities(LifecycleActive)
	for _, phase := range []string{"announced", "deposits_paused", "trading_halted", "converting", "completed"} {
		assert.Equal(t, active, MigrationCapabilities(phase, false), "an optional migration changes nothing at %s", phase)
	}
	assert.Equal(t, active, MigrationCapabilities("announced", true))
	paused := MigrationCapabilities("deposits_paused", true)
	assert.False(t, paused.Deposit)
	assert.False(t, paused.Withdraw)
	assert.True(t, paused.Sell)
	assert.Equal(t, models.CurrencyCapabilities{Listed: true}, MigrationCapabilities("converting", true))
	assert.Equal(t, models.CurrencyCapabilities{}, MigrationCapabilities("completed", true))
}

func TestValidDecimal(t *testing.T) {
	for _, ok := range []string{"1", "0.01", "24000", "0.433350", "0", "12345678901234567890.123456789012345678"} {
		assert.True(t, ValidDecimal(ok), ok)
	}
	for _, bad := range []string{"", ".", "1.", ".5", "-1", "1e3", "1,5", "0x10", "1.1234567890123456789", "123456789012345678901"} {
		assert.False(t, ValidDecimal(bad), bad)
	}
}
