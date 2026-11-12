package auth

import (
	"sync/atomic"

	"github.com/mdastpak/crypto-exchange/internal/config"
)

// The configuration that the token and session functions of this package
// read: the jwt section, which the composition root installs with
// Configure (#48). The package reads no global configuration.
var configured atomic.Pointer[config.Configuration]

// noSettings is what the package reads before Configure: no issuer, and
// every lifetime zero.
var noSettings config.Configuration

// Configure installs the configuration of the process, which the token and
// session functions read from then on. The server calls it before it issues
// or checks a token; changes made through cfg later are seen.
func Configure(cfg *config.Configuration) { configured.Store(cfg) }

// settings returns the installed configuration.
func settings() *config.Configuration {
	if cfg := configured.Load(); cfg != nil {
		return cfg
	}
	return &noSettings
}
