package middleware

import (
	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
)

// The tests set the jwt section of the global configuration, which the
// server installs with auth.Configure.
func init() { auth.Configure(&config.Config) }
