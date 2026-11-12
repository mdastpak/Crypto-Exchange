package public

import (
	"database/sql"

	"github.com/mdastpak/crypto-exchange/internal/auth"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/signin"
)

// The tests set the jwt section of the global configuration, which the
// server installs with auth.Configure.
func init() { auth.Configure(&config.Config) }

// testDB is the database of a handler that NewHandler built over test
// services.
func testDB(h *PublicHandler) *sql.DB { return h.Accounts.(*signin.Accounts).DB }
