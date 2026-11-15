package app

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"log"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/price"
	"github.com/mdastpak/crypto-exchange/internal/priceoverride"
)

// overridePoll is how often each instance reads the price overrides again:
// a change on another instance applies here within it.
const overridePoll = time.Second

// startPrices starts this instance's copy of the price overrides (#59), and
// returns the reader of the reference prices, which takes an override in
// force first and the price engine's price in Redis otherwise. Every
// instance runs one; the overrides are in PostgreSQL, sealed, so that no
// write to Redis can set a price.
func startPrices(conn *sql.DB, client redis.UniversalClient, handlers *InfraHandlers) (*price.Reader, worker, error) {
	verifier, err := priceVerifier(config.Config.Price)
	if err != nil {
		return nil, worker{}, err
	}
	watcher := priceoverride.NewWatcher(conn, overridePoll, log.Default())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := watcher.Refresh(ctx); err != nil {
		log.Printf("Failed to read the price overrides at startup; reading them again every %s: %v", overridePoll, err)
	}
	cancel()
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.Run(runCtx)
	}()
	reader := price.NewReader(client, watcher, price.DefaultMaxAge).Verify(verifier)
	handlers.Admin.Prices = reader
	handlers.Admin.OverrideWatcher = watcher
	return reader, worker{"price override watcher", func(ctx context.Context) error {
		stop()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}, nil
}

// priceVerifier returns the verifier of the prices in Redis (#450), with
// the public keys of price.verify_keys, or nil when the prices are not
// verified, outside release mode only.
func priceVerifier(cfg config.PriceConfig) (*price.Verifier, error) {
	if !cfg.Verifies() {
		log.Printf("WARNING: price.signatures is off: every price in Redis is trusted, and whoever can write to Redis sets the price of OTC quotes and delisting settlements. Set price.verify_keys outside development.")
		return nil, nil
	}
	keys := make([]ed25519.PublicKey, 0, len(cfg.VerifyKeys))
	for _, path := range cfg.VerifyKeys {
		key, err := price.LoadVerifyKey(path)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	v := price.NewVerifier(keys...)
	log.Printf("Prices in Redis are verified with the keys %v", v.IDs())
	return v, nil
}
