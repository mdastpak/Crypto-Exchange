package app

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/redis/go-redis/v9"

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
func startPrices(conn *sql.DB, client redis.UniversalClient, handlers *InfraHandlers) (*price.Reader, worker) {
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
	reader := price.NewReader(client, watcher, price.DefaultMaxAge)
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
	}}
}
