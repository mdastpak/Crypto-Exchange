package platform

import (
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
)

// Settings are the platform settings of the configuration.
type Settings struct {
	Options
	// Approval are the features whose active lockdown needs a second
	// administrator to lift.
	Approval []Feature
}

// SettingsOf reads the platform section of c,
// which config.ValidateServer has bounded. It reports every unknown
// feature at once.
func SettingsOf(c *config.Configuration) (Settings, error) {
	cfg := c.Platform
	failClosed, err1 := parseFeatures("platform.fail_closed", cfg.FailClosed, DefaultFailClosed)
	approval, err2 := parseFeatures("platform.lift_approval", cfg.LiftApproval, DefaultApproval)
	if err := errors.Join(err1, err2); err != nil {
		return Settings{}, err
	}
	return Settings{
		Options: Options{
			PollInterval: time.Duration(cfg.PollIntervalMs) * time.Millisecond,
			StaleAfter:   time.Duration(cfg.StaleAfterSeconds) * time.Second,
			FailClosed:   failClosed,
		},
		Approval: approval,
	}, nil
}

// parseFeatures reads a list of features: unset is def, and ["none"] is
// none.
func parseFeatures(key string, raw []string, def []Feature) ([]Feature, error) {
	if raw == nil {
		return def, nil
	}
	features := []Feature{}
	if len(raw) == 1 && raw[0] == "none" {
		return features, nil
	}
	for _, name := range raw {
		f := Feature(name)
		if !f.Valid() {
			return nil, fmt.Errorf("%s: %q is not a feature; use none, or some of %s", key, name, featureList())
		}
		features = append(features, f)
	}
	return features, nil
}
