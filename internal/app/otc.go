package app

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/ledger"
	"github.com/mdastpak/crypto-exchange/internal/otc"
	"github.com/mdastpak/crypto-exchange/internal/otc/routing"
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
	executor := &otc.Executor{
		DB: db, Store: store, Ledger: ledger.Ledger{DB: db}, Verification: levels, Limits: limits, Committed: committed,
		RequireLimits: settings.Server.Mode == "release",
		Legal:         func() config.LegalConfig { return settings.Legal },
	}
	h.User.Trades = executor
	if settings.OTC.Routing.Enabled {
		r, err := otcRouting(db, settings, levels)
		if err != nil {
			return err
		}
		s.Route(r)
		executor.Routing = r
	}
	return nil
}

// otcRouting routes the quotes and their executions by the rules (#386),
// which each instance keeps for otc.routing.cache_seconds.
func otcRouting(db *sql.DB, settings *config.Configuration, levels verification.Store) (*otc.Routing, error) {
	current := func() *config.Configuration { return settings }
	rules := &routing.Store{DB: db, Config: current}
	ttl := time.Duration(settings.OTC.Routing.CacheSeconds) * time.Second
	router, err := otc.NewRouter(db, current, routing.NewCache(rules, ttl), rules)
	if err != nil {
		return nil, fmt.Errorf("otc routing: %w", err)
	}
	return &otc.Routing{
		Router: router, Region: settings.Region(),
		Level: func(ctx context.Context, userHK string) (int, error) {
			st, err := levels.Level(ctx, db, userHK)
			return int(st.Level), err
		},
	}, nil
}
