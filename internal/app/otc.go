package app

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/redis/go-redis/v9"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/otc"
	"github.com/mdastpak/crypto-exchange/internal/verification"
)

// useOTC gives the user routes the OTC quotes (#61), priced from the
// reference prices of startPrices, which it runs after, and checked against
// the verification levels. Without the prices or the levels, the route
// answers otc_unavailable. The quotes are signed with the otp-mac data key,
// which every instance shares; without data keys, a quote can be taken
// only on the instance that gave it, as a development setup with one
// instance needs. The executions of the quotes (#62) share the store, so a
// quote is taken once on any instance, and nudge the outbox relay with
// committed after each trade.
func useOTC(h *InfraHandlers, db *sql.DB, client redis.UniversalClient, settings *config.Configuration, committed func()) error {
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
	checks := otc.DBChecks{DB: db, Verification: levels, Precisions: &otc.PrecisionCache{}}
	s, err := otc.New(settings.OTC, h.Prices, h.Public.Markets, checks, store, otc.NewMetrics(markets))
	if err != nil {
		return fmt.Errorf("otc: %w", err)
	}
	h.User.Quotes = s
	limits, err := otc.ParseInventoryLimits(settings.OTC.InventoryLimits)
	if err != nil {
		return err
	}
	h.User.Trades = &otc.Executor{
		DB: db, Store: store, Ledger: ledger.Ledger{DB: db}, Verification: levels, Limits: limits, Committed: committed,
		Legal: func() config.LegalConfig { return settings.Legal },
	}
	return nil
}
