package userauth

import (
	"sync/atomic"

	"github.com/mdastpak/crypto-exchange/internal/config"
)

// The configuration that this package reads its security settings from,
// which the composition root installs with Configure (#48). The package
// reads no global configuration.
var configured atomic.Pointer[config.Configuration]

// noSettings is what the package reads before Configure: every setting
// unset, so that each takes its default.
var noSettings config.Configuration

// Configure installs the configuration of the process, whose security
// section the stores and the guard read from then on; changes made through
// cfg later are seen.
func Configure(cfg *config.Configuration) { configured.Store(cfg) }

// settings returns the installed configuration.
func settings() *config.Configuration {
	if cfg := configured.Load(); cfg != nil {
		return cfg
	}
	return &noSettings
}
