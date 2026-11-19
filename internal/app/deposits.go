package app

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/deposits"
	"github.com/mdastpak/crypto-exchange/internal/tron"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// useDeposits gives the users their deposit addresses (#68) on each enabled
// custody network with an extended public key: TRON for now. An xpub that
// does not parse, or an extended private key, stops the server; a network
// without one takes no deposits, and says so.
func useDeposits(h *InfraHandlers, db *sql.DB, settings *config.Configuration) error {
	levels, ok := h.User.Verification.(verification.Store)
	if !ok {
		return nil
	}
	networks := map[string]deposits.Deriver{}
	for i, p := range settings.Custody.Providers {
		if !p.Enabled || p.Network != "tron" {
			continue
		}
		if p.XPub == "" {
			log.Printf("custody.providers[%d] (tron) has no xpub: the users get no TRON deposit address", i)
			continue
		}
		x, err := tron.ParseXPub(p.XPub)
		if err != nil {
			return fmt.Errorf("custody.providers[%d].xpub: %w", i, err)
		}
		networks["tron"] = x
	}
	if len(networks) == 0 {
		return nil
	}
	h.User.Deposits = deposits.Service{DB: db, Networks: networks, Levels: levels}
	return nil
}
