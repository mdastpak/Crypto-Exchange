package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestWithdrawals_TheDefaultsOfTheDestinations(t *testing.T) {
	var w WithdrawalsConfig
	assert.Equal(t, DefaultMaxDestinations, w.Destinations())
	assert.Zero(t, w.DestinationWait(true), "the user's own accounts wait for nothing")
	assert.Equal(t, 24*time.Hour, w.DestinationWait(false), "an address waits a day")

	zero, three := 0, 3
	w = WithdrawalsConfig{MaxDestinations: 5, CryptoDestinationWaitHours: &zero, FiatDestinationWaitHours: &three}
	assert.Equal(t, 5, w.Destinations())
	assert.Zero(t, w.DestinationWait(false), "a wait of 0 is a setting, not a missing one")
	assert.Equal(t, 3*time.Hour, w.DestinationWait(true))
	assert.Equal(t, 10*time.Second, BankOwnershipConfig{}.Timeout())
}
