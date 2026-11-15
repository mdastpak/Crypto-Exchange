package app

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/redis/go-redis/v9"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/otc"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// useOTC gives the user routes the OTC quotes (#61), priced from the
// reference prices of startPrices, which it runs after, and checked against
// the verification levels. Without the prices or the levels, the route
// answers otc_unavailable. The quotes are signed with the otp-mac data key,
// which every instance shares; without data keys, a quote can be taken
// only on the instance that gave it, as a development setup with one
// instance needs.
func useOTC(h *InfraHandlers, db *sql.DB, client redis.UniversalClient, settings *config.Configuration) error {
	levels, ok := h.User.Verification.(verification.Store)
	if h.Prices == nil || h.Public == nil || h.Public.Markets == nil || !ok {
		return nil
	}
	var dataKeys otc.DataKeys
	if h.DataKeys != nil {
		dataKeys = h.DataKeys
	} else {
		log.Printf("OTC quotes are signed with a key of this instance, without data keys: run one instance only")
	}
	store, err := otc.NewStore(client, dataKeys)
	if err != nil {
		return err
	}
	markets := make([]string, 0, len(settings.OTC.Markets))
	for symbol := range settings.OTC.Markets {
		markets = append(markets, symbol)
	}
	checks := otc.DBChecks{DB: db, Verification: levels}
	s, err := otc.New(settings.OTC, h.Prices, h.Public.Markets, checks, store, otc.NewMetrics(markets))
	if err != nil {
		return fmt.Errorf("otc: %w", err)
	}
	h.User.Quotes = s
	return nil
}
