// Command market-worker collects the global market data catalog (#262):
// the metadata of assets from sources such as CoinGecko and CoinMarketCap,
// and the markets of global and Iranian exchanges, as unverified entries
// apart from the currencies listed on this exchange. See
// docs/market-data.md.
//
// It runs apart from the server, so that it can run only on the nodes that
// reach the sources, and so that a failing or blocked source never touches
// the trading API. Several workers may run at once; each job run happens on
// one of them.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/platform"
)

var (
	configPath     string
	migrationsPath string
)

func init() {
	flag.StringVar(&configPath, "config", "config/config.yaml", "Path to config file")
	flag.StringVar(&migrationsPath, "migrations", "file://internal/db/migrations", "Path to migrations")
}

// shutdownTimeout bounds the stop of the worker.
const shutdownTimeout = 30 * time.Second

func main() {
	flag.Parse()
	if err := run(); err != nil {
		log.Fatal(err)
	}
	log.Println("Market worker stopped gracefully")
}

// run starts the worker and runs it until a SIGINT or SIGTERM.
func run() error {
	config.SetConfigPath(configPath)
	config.SetMigrationsPath(migrationsPath)
	config.LoadConfig()
	if err := config.ValidateMarketWorker(); err != nil {
		return fmt.Errorf("invalid configuration:\n%v", err)
	}

	// pg_stat_activity tells the sessions of each worker apart (#230).
	db.ApplicationName = applicationName()
	// The migrations run here too, under the same lock as the server's,
	// unless postgres.migrations.mode is external.
	postgres, err := db.NewPostgresService()
	if err != nil {
		return fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}
	defer postgres.Close()

	cfg := config.Config.MarketData
	var stopJobs func(ctx context.Context) error
	if cfg.Enabled {
		if stopJobs, err = startJobs(postgres.DB, db.ApplicationName, jobSpecs()); err != nil {
			return err
		}
		log.Printf("Market worker started in %s mode", cfg.Mode)
	} else {
		log.Println("market_data.enabled is false: the worker runs no job and only answers its probes")
	}

	probes := &probes{ping: postgres.Ping, stopJobs: stopJobs}
	srv := &http.Server{
		Addr:              cfg.HealthAddr,
		Handler:           probes.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, srv, probes)
}

// serve answers the probes until ctx is done or the server fails, and then
// stops: readiness fails first, and the server closes.
func serve(ctx context.Context, srv *http.Server, p *probes) error {
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()

	var failure error
	select {
	case <-ctx.Done():
		log.Println("Stopping the market worker...")
	case err := <-served:
		failure = fmt.Errorf("the probe server stopped: %w", err)
	}
	p.draining.Store(true)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// The runs in progress finish, or are cancelled at the deadline and are
	// due at once for another worker.
	if p.stopJobs != nil {
		if err := p.stopJobs(shutdownCtx); err != nil {
			failure = errors.Join(failure, err)
		}
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		failure = errors.Join(failure, err)
	}
	return failure
}

// jobSpecs are the jobs of the worker. The sources and their jobs come
// with #265 to #268.
func jobSpecs() []jobs.Spec { return nil }

// startJobs starts the platform watcher, whose lockdown of market_data
// pauses the jobs on every worker, and the runner of the jobs. It returns
// the function that stops both.
func startJobs(conn *sql.DB, node string, specs []jobs.Spec) (func(ctx context.Context) error, error) {
	settings, err := platform.SettingsFromConfig()
	if err != nil {
		return nil, fmt.Errorf("invalid platform configuration: %w", err)
	}
	watcher := platform.NewWatcher(conn, settings.Options)
	cfg := config.Config.MarketData
	runner, err := jobs.NewRunner(conn, jobs.Options{
		Node:          node,
		Mode:          cfg.Mode,
		Poll:          time.Duration(cfg.PollSeconds) * time.Second,
		Lease:         time.Duration(cfg.LeaseSeconds) * time.Second,
		MaxConcurrent: cfg.MaxConcurrentJobs,
		RetryBase:     time.Duration(cfg.RetryBaseSeconds) * time.Second,
		Paused:        func() bool { return watcher.Paused(platform.FeatureMarketData) },
	}, specs...)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watcher.Start(ctx)
	runner.Start()
	return func(ctx context.Context) error {
		return errors.Join(runner.Stop(ctx), watcher.Stop(ctx))
	}, nil
}

// applicationName is the application_name of a worker:
// crypto-exchange-market-worker/<host>.
func applicationName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return "crypto-exchange-market-worker/" + host
}
