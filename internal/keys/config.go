package keys

import (
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
)

// FromConfig returns the provider that cfg selects, or nil when
// keys.provider is empty.
func FromConfig(cfg config.KeysConfig) (Provider, error) {
	switch cfg.Provider {
	case "":
		return nil, nil
	case config.KeysProviderLocal:
		p, err := NewLocalProvider(cfg.Local.KEK)
		if err != nil {
			return nil, err
		}
		return p, nil
	case config.KeysProviderTransit:
		p, err := NewTransitProvider(TransitConfig{
			Address:   cfg.Transit.Address,
			Token:     cfg.Transit.Token,
			TokenFile: cfg.Transit.TokenFile,
			Mount:     cfg.Transit.Mount,
			Key:       cfg.Transit.Key,
			CACert:    cfg.Transit.CACert,
			Timeout:   time.Duration(cfg.Transit.Timeout) * time.Second,
		})
		if err != nil {
			return nil, err
		}
		return p, nil
	}
	return nil, fmt.Errorf("unknown keys.provider %q", cfg.Provider)
}
