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
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/candles"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/catalog"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source/coingecko"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source/coinmarketcap"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source/exchange"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/stream"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/tickers"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/tiers"
	"github.com/mdastpak/crypto-exchange/internal/platform"
)

var (
	configPath     string
	migrationsPath string
)

func init() {
	// UTC whatever the host's time zone, as the server (#339).
	time.Local = time.UTC
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
	config.RequireConfigFile(configFlagSet())
	config.SetMigrationsPath(migrationsPath)
	config.LoadConfig()
	if err := config.ValidateMarketWorker(); err != nil {
		return fmt.Errorf("invalid configuration:\n%v", err)
	}

	// pg_stat_activity tells the sessions of each worker apart (#230).
	db.ApplicationName = applicationName()
	// The migrations run here too, under the same lock as the server's,
	// unless postgres.migrations.mode is external.
	postgres, err := db.NewPostgresService(dbOptions())
	if err != nil {
		return fmt.Errorf("failed to connect to PostgreSQL: %w", err)
	}
	defer postgres.Close()

	cfg := config.Config.MarketData
	var stopJobs func(ctx context.Context) error
	if cfg.Enabled {
		specs, err := jobSpecs(postgres.DB)
		if err != nil {
			return fmt.Errorf("invalid market data jobs: %w", err)
		}
		streams, err := streamSpecs(postgres.DB)
		if err != nil {
			return fmt.Errorf("invalid market data streams: %w", err)
		}
		if stopJobs, err = startJobs(postgres.DB, db.ApplicationName, specs, streams); err != nil {
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

// catalogRunTimeout bounds one run of the catalog sync of a source.
const catalogRunTimeout = time.Hour

// jobSpecs are the jobs of the worker: those of the sources that this node
// reaches. Another node may run the jobs of the others.
func jobSpecs(conn *sql.DB) ([]jobs.Spec, error) {
	cfg := config.Config.MarketData
	store := marketdata.NewStore(conn)
	var specs []jobs.Spec
	for _, name := range cfg.Catalog.Priority {
		if !cfg.Reachable(name) {
			continue
		}
		src, ids, err := catalogSource(conn, name)
		if err != nil {
			return nil, err
		}
		job, err := catalog.New(src, store, catalog.Options{
			Priority:        cfg.Catalog.Priority,
			Coverage:        cfg.Catalog.Coverage,
			Top:             cfg.Catalog.Top,
			IDs:             ids,
			DetailsPerRun:   cfg.Catalog.DetailsPerRun,
			DetailsMaxAge:   time.Duration(cfg.Catalog.DetailsMaxAgeDays) * 24 * time.Hour,
			DeactivateAfter: time.Duration(cfg.Catalog.DeactivateAfterDays) * 24 * time.Hour,
		})
		if err != nil {
			return nil, err
		}
		specs = append(specs, jobs.Spec{Job: job, Interval: time.Duration(cfg.Catalog.IntervalHours) * time.Hour, Timeout: catalogRunTimeout})
	}
	// One ticker job per venue, so that one venue never holds up another.
	for _, name := range exchange.Names {
		if !cfg.Reachable(name) {
			continue
		}
		settings, _ := exchange.Settings(name)
		client, err := source.ClientFromConfig(&config.Config, conn, name, settings)
		if err != nil {
			return nil, err
		}
		src, err := exchange.New(name, client)
		if err != nil {
			return nil, err
		}
		interval := time.Duration(cfg.Tickers.IntervalSeconds) * time.Second
		specs = append(specs, jobs.Spec{Job: tickers.New(src, store, tickers.Options{
			MapRankLimit:    cfg.Tickers.MapRankLimit,
			DeactivateAfter: time.Duration(cfg.Tickers.DeactivatePairsAfterHours) * time.Hour,
		}), Interval: interval, Timeout: max(interval, time.Minute)})
		candleSpecs, err := candleJobs(store, src)
		if err != nil {
			return nil, err
		}
		specs = append(specs, candleSpecs...)
	}
	// The tiers of the pairs, on any worker (#279).
	tierJob, err := tiers.New(store, tiers.Options{
		HotAssets:         cfg.Tiers.HotAssets,
		HotListed:         cfg.Tiers.HotListed == nil || *cfg.Tiers.HotListed,
		Quotes:            cfg.Candles.Quotes,
		ActiveTop:         cfg.Tiers.ActiveTop,
		HysteresisPercent: cfg.Tiers.HysteresisPercent,
	})
	if err != nil {
		return nil, err
	}
	specs = append(specs, jobs.Spec{Job: tierJob, Interval: time.Duration(cfg.Tiers.IntervalHours) * time.Hour, Timeout: 5 * time.Minute})
	// The partitions of the candles, on any worker.
	var retentions []candles.Retention
	for _, name := range marketdata.Resolutions {
		r := cfg.Candles.Resolutions[name]
		retentions = append(retentions, candles.Retention{Resolution: name, Ensure: r.Enabled,
			Keep: time.Duration(r.RetentionDays) * 24 * time.Hour})
	}
	specs = append(specs, jobs.Spec{Job: candles.NewPartitions(store, retentions, nil),
		Interval: candlePartitionsInterval, Timeout: 5 * time.Minute})
	return specs, nil
}

// streamSpecs are the streams of the venues that have one and that this
// node reaches (#281). A run holds its stream for run_minutes; the next is
// due a second after, on whichever worker claims it.
func streamSpecs(conn *sql.DB) ([]jobs.Spec, error) {
	cfg := config.Config.MarketData
	if cfg.Streams.Enabled != nil && !*cfg.Streams.Enabled {
		return nil, nil
	}
	protocols := stream.Protocols()
	var specs []jobs.Spec
	for _, name := range exchange.Names {
		proto, ok := protocols[name]
		if !ok || !cfg.Reachable(name) {
			continue
		}
		proxy := ""
		if cfg.Route(name) == config.EgressProxy {
			proxy = cfg.Egress.ProxyURL
		}
		dial, err := stream.NewDialer(proxy, time.Duration(cfg.Streams.QuietSeconds)*time.Second)
		if err != nil {
			return nil, fmt.Errorf("stream %s: %w", name, err)
		}
		job := stream.New(proto, dial, marketdata.NewStore(conn), stream.Options{
			Tiers:    cfg.Streams.Tiers,
			MaxPairs: cfg.Streams.MaxPairs,
			RunFor:   time.Duration(cfg.Streams.RunMinutes) * time.Minute,
			Flush:    time.Duration(cfg.Streams.FlushMillis) * time.Millisecond,
		})
		specs = append(specs, jobs.Spec{Job: job, Interval: time.Second, Timeout: job.Timeout()})
	}
	return specs, nil
}

// candleRunTimeout bounds one run of the candles of a venue.
const candleRunTimeout = 30 * time.Minute

// candlePartitionsInterval is the time between two runs of the partitions
// job, which creates the next period's partitions well before it starts.
const candlePartitionsInterval = 6 * time.Hour

// candleJobs returns the candle jobs of a venue, one per enabled resolution
// that the venue gives.
func candleJobs(store *marketdata.Store, src source.VenueSource) ([]jobs.Spec, error) {
	cs, ok := src.(source.CandleSource)
	if !ok {
		return nil, nil
	}
	cfg := config.Config.MarketData.Candles
	var specs []jobs.Spec
	for _, name := range marketdata.Resolutions {
		r := cfg.Resolutions[name]
		if !r.Enabled {
			continue
		}
		if s, ok := src.(interface{ Supports(source.Interval) bool }); ok && !s.Supports(source.Interval(name)) {
			continue
		}
		job, err := candles.New(cs, store, candles.Options{
			Resolution:     name,
			Quotes:         cfg.Quotes,
			Tiers:          r.Tiers,
			MaxPairs:       cfg.MaxPairsPerVenue,
			RequestsPerRun: cfg.RequestsPerRun,
			Backfill:       time.Duration(r.BackfillDays) * 24 * time.Hour,
			Retention:      time.Duration(r.RetentionDays) * 24 * time.Hour,
		})
		if err != nil {
			return nil, err
		}
		specs = append(specs, jobs.Spec{Job: job, Interval: time.Duration(r.IntervalMinutes) * time.Minute, Timeout: candleRunTimeout})
	}
	return specs, nil
}

// catalogSource returns the named catalog source on its client, and the ids
// of its "list" coverage.
func catalogSource(conn *sql.DB, name string) (source.CatalogSource, []string, error) {
	switch name {
	case coingecko.Name:
		client, err := source.ClientFromConfig(&config.Config, conn, name, coingecko.Settings(config.Config.MarketData.Sources[name].BaseURL))
		if err != nil {
			return nil, nil, err
		}
		return coingecko.New(client), coingecko.DevelopmentIDs, nil
	case coinmarketcap.Name:
		client, err := source.ClientFromConfig(&config.Config, conn, name, coinmarketcap.Settings)
		if err != nil {
			return nil, nil, err
		}
		return coinmarketcap.New(client), coinmarketcap.DevelopmentIDs, nil
	}
	return nil, nil, fmt.Errorf("%s is not a catalog source", name)
}

// startJobs starts the platform watcher, whose lockdown of market_data
// pauses the jobs on every worker, and the runner of the jobs. It returns
// the function that stops both.
func startJobs(conn *sql.DB, node string, specs, streams []jobs.Spec) (func(ctx context.Context) error, error) {
	settings, err := platform.SettingsOf(&config.Config)
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
		// The node runs only the jobs of the sources its configuration
		// reaches (#265).
		Reachable: cfg.Reachable,
	}, specs...)
	if err != nil {
		return nil, err
	}
	// The streams hold their runs for long, so they have runners of their
	// own, with slots of their own: they never take the slots of the jobs,
	// and one worker holds at most max_per_worker of them.
	var streamRunner *jobs.Runner
	if len(streams) > 0 {
		streamRunner, err = jobs.NewRunner(conn, jobs.Options{
			Node:          node,
			Mode:          cfg.Mode,
			Poll:          time.Duration(cfg.PollSeconds) * time.Second,
			Lease:         time.Duration(cfg.LeaseSeconds) * time.Second,
			MaxConcurrent: cfg.Streams.MaxPerWorker,
			RetryBase:     time.Duration(cfg.RetryBaseSeconds) * time.Second,
			Paused:        func() bool { return watcher.Paused(platform.FeatureMarketData) },
			Reachable:     cfg.Reachable,
		}, streams...)
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watcher.Start(ctx)
	runner.Start()
	if streamRunner != nil {
		streamRunner.Start()
	}
	return func(ctx context.Context) error {
		var streamsStopped error
		if streamRunner != nil {
			streamsStopped = streamRunner.Stop(ctx)
		}
		return errors.Join(runner.Stop(ctx), streamsStopped, watcher.Stop(ctx))
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

// configFlagSet reports whether -config was given: a named file that does
// not exist stops the worker instead of falling back to the example (#306).
func configFlagSet() (set bool) {
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == "config" })
	return set
}

// dbOptions are the options of the db package: the configuration of this
// process, and its migrations.
func dbOptions() db.Options {
	return db.Options{Config: &config.Config, MigrationsPath: config.MigrationsPath()}
}
