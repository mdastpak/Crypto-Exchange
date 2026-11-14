package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAntiPhishing_DefaultsAndOff loads the defaults of the reminders to
// change the anti-phishing code (#375), and a file that turns the age
// bound off and keeps the uses bound.
func TestAntiPhishing_DefaultsAndOff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	base, err := os.ReadFile(configFile("config.compose.yaml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, base, 0o600))
	require.NoError(t, loadFile(t, path, nil))
	assert.Equal(t, AntiPhishingRotation{MaxAgeDays: 180, MaxUses: 200, ReminderIntervalDays: 30, CheckIntervalMinutes: 60},
		Config.AntiPhishing.Rotation)
	assert.True(t, Config.AntiPhishing.Rotation.Enabled())
	assert.Equal(t, DefaultAntiPhishingReminderSubject, Config.Notifications.Email.AntiPhishingReminderSubject)

	file := string(base) + `
anti_phishing:
  rotation:
    max_age_days: -1
    max_uses: 50
`
	require.NoError(t, os.WriteFile(path, []byte(file), 0o600))
	require.NoError(t, loadFile(t, path, nil))
	r := Config.AntiPhishing.Rotation
	assert.Equal(t, -1, r.MaxAgeDays, "a negative bound is off, not a missing one")
	assert.Equal(t, 50, r.MaxUses)
	assert.True(t, r.Enabled())
	assert.False(t, AntiPhishingRotation{MaxAgeDays: -1, MaxUses: -1}.Enabled())
}
