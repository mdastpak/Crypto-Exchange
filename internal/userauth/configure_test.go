package userauth

import "github.com/mdastpak/crypto-exchange/internal/config"

// The tests set the security section of the global configuration, which
// the server installs with Configure.
func init() { Configure(&config.Config) }
