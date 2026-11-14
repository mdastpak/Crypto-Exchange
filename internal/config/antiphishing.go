package config

// AntiPhishingConfig configures the anti-phishing codes of users (#375).
// See docs/configuration.md#anti-phishing.
type AntiPhishingConfig struct {
	// Rotation sets when the exchange suggests that a user change their
	// code: once it is MaxAgeDays old, or once MaxUses emails have shown
	// it, whichever comes first. A reminder goes by email and SMS, and
	// then again every ReminderIntervalDays until the code changes.
	Rotation AntiPhishingRotation `yaml:"rotation"`
}

// AntiPhishingRotation is the reminder to change an anti-phishing code.
type AntiPhishingRotation struct {
	// MaxAgeDays is the age at which a code is due for a change. 0 takes
	// DefaultAntiPhishingMaxAgeDays; a negative value never suggests a
	// change for age.
	MaxAgeDays int `yaml:"max_age_days"`
	// MaxUses is the number of emails after which a code is due for a
	// change. 0 takes DefaultAntiPhishingMaxUses; a negative value never
	// suggests a change for use.
	MaxUses int `yaml:"max_uses"`
	// ReminderIntervalDays is the time between two reminders to one user.
	// 0 takes DefaultAntiPhishingReminderIntervalDays.
	ReminderIntervalDays int `yaml:"reminder_interval_days"`
	// CheckIntervalMinutes is how often an instance looks for the codes
	// that are due. 0 takes DefaultAntiPhishingCheckIntervalMinutes.
	CheckIntervalMinutes int `yaml:"check_interval_minutes"`
}

// The defaults of anti_phishing.rotation.
const (
	DefaultAntiPhishingMaxAgeDays           = 180
	DefaultAntiPhishingMaxUses              = 200
	DefaultAntiPhishingReminderIntervalDays = 30
	DefaultAntiPhishingCheckIntervalMinutes = 60
)

// Enabled reports whether codes are ever due for a change.
func (r AntiPhishingRotation) Enabled() bool {
	return r.MaxAgeDays > 0 || r.MaxUses > 0
}

// applyAntiPhishingDefaults gives the unset bounds their defaults.
func (c *Configuration) applyAntiPhishingDefaults() {
	r := &c.AntiPhishing.Rotation
	if r.MaxAgeDays == 0 {
		r.MaxAgeDays = DefaultAntiPhishingMaxAgeDays
	}
	if r.MaxUses == 0 {
		r.MaxUses = DefaultAntiPhishingMaxUses
	}
	if r.ReminderIntervalDays == 0 {
		r.ReminderIntervalDays = DefaultAntiPhishingReminderIntervalDays
	}
	if r.CheckIntervalMinutes == 0 {
		r.CheckIntervalMinutes = DefaultAntiPhishingCheckIntervalMinutes
	}
}

// antiPhishingProblems reports intervals that are not positive.
func (c *Configuration) antiPhishingProblems(p *problems) {
	r := c.AntiPhishing.Rotation
	if r.ReminderIntervalDays < 1 {
		p.add("anti_phishing.rotation.reminder_interval_days", "is %d; it must be at least 1", r.ReminderIntervalDays)
	}
	if r.CheckIntervalMinutes < 1 {
		p.add("anti_phishing.rotation.check_interval_minutes", "is %d; it must be at least 1", r.CheckIntervalMinutes)
	}
}
