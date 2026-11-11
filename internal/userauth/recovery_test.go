package userauth

import (
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"

	"github.com/stretchr/testify/assert"
)

func TestRecoveryPlan(t *testing.T) {
	saved := config.Config.Security.Recovery
	t.Cleanup(func() { config.Config.Security.Recovery = saved })

	config.Config.Security.Recovery.Mode = ""
	mode, steps := RecoveryPlan(true)
	assert.Equal(t, RecoveryStandard, mode)
	assert.Empty(t, steps)

	config.Config.Security.Recovery.Mode = RecoveryStrict
	config.Config.Security.Recovery.Steps = nil
	mode, steps = RecoveryPlan(true)
	assert.Equal(t, RecoveryStrict, mode)
	assert.Equal(t, []string{StepNationalID, StepMobileOwnership}, steps, "strict without steps checks everything")

	config.Config.Security.Recovery.Steps = []string{StepNationalID}
	_, steps = RecoveryPlan(true)
	assert.Equal(t, []string{StepNationalID}, steps)

	// An account without a verified national ID has no identity to check.
	mode, steps = RecoveryPlan(false)
	assert.Equal(t, RecoveryStandard, mode)
	assert.Empty(t, steps)
}

func TestSplitList(t *testing.T) {
	assert.Equal(t, []string{}, splitList(""))
	assert.Equal(t, []string{"a", "b"}, splitList("a,b"))
}
