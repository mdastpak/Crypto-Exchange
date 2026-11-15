// Command price-aggregator is the price engine of the exchange (#58): it
// follows the books and trades of the configured providers, and computes a
// reference price per market every price_aggregator.aggregation_interval_ms.
// See docs/price-engine.md.
//
// It runs apart from the server, so that it can run only on the nodes that
// reach the providers, and so that a slow or blocked provider never touches
// the trading API. It needs no database: several replicas may run at once,
// each following the providers and computing the same prices on its own,
// and writing them to Redis; one at a time publishes the updates (#59).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/db"
	"github.com/mdastpak/crypto-exchange/internal/logging"
	"github.com/mdastpak/crypto-exchange/internal/metrics"
	"github.com/mdastpak/crypto-exchange/internal/price"
	"github.com/mdastpak/crypto-exchange/internal/rabbitmq"
)

var configPath string

func init() {
	// UTC whatever the host's time zone, as the server (#339).
	time.Local = time.UTC
	flag.StringVar(&configPath, "config", "config/config.yaml", "Path to config file")
}

// shutdownTimeout bounds the stop of the aggregator.
const shutdownTimeout = 10 * time.Second

func main() {
	flag.Parse()
	if err := run(); err != nil {
		log.Fatal(err)
	}
	log.Println("Price aggregator stopped gracefully")
}

// run starts the aggregator and runs it until a SIGINT or SIGTERM.
func run() error {
	config.SetConfigPath(configPath)
	config.RequireConfigFile(configFlagSet())
	config.LoadConfig()
	closeLog, err := logging.Setup(&config.Config)
	if err != nil {
		return fmt.Errorf("invalid logging configuration: %w", err)
	}
	defer closeLog()
	if err := config.ValidatePriceAggregator(); err != nil {
		return fmt.Errorf("invalid configuration:\n%v", err)
	}

	p := &probes{}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var wg sync.WaitGroup
	if config.Config.PriceAggregator.Enabled {
		agg, feeds, err := price.FromConfig(&config.Config, log.Default())
		if err != nil {
			return fmt.Errorf("invalid price providers: %w", err)
		}
		interval := time.Duration(config.Config.PriceAggregator.AggregationInterval) * time.Millisecond
		publisher, closeTargets, err := newPublisher()
		if err != nil {
			return err
		}
		defer closeTargets()
		var sinks []price.Sink
		if publisher != nil {
			sinks = append(sinks, publisher)
			wg.Go(func() { publisher.Run(ctx) })
		}
		p.service = price.NewService(agg, feeds, interval, sinks...)
		wg.Go(func() { p.service.Run(ctx) })
		log.Printf("Price aggregator started: %d providers, every %s", len(feeds), interval)
	} else {
		log.Println("price_aggregator.enabled is false: the aggregator follows no provider and only answers its probes")
	}

	srv := &http.Server{
		Addr:              config.Config.PriceAggregator.HealthAddr,
		Handler:           p.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	err = serve(ctx, srv, p)
	stop()
	wg.Wait()
	return err
}

// newPublisher returns the publisher of the prices to Redis and RabbitMQ
// (#59), or nil when the aggregator publishes nowhere, and the function that
// closes their connections.
func newPublisher() (*price.Publisher, func(), error) {
	a := config.Config.PriceAggregator
	if !a.PublishToRedis {
		return nil, func() {}, nil
	}
	redisService, err := db.NewRedisService(&config.Config)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}
	closers := []func(){func() { _ = redisService.Close() }}
	opts := price.PublisherOptions{
		Redis:        redisService.Client,
		CacheTTL:     time.Duration(a.CacheTTLMs) * time.Millisecond,
		Stream:       price.DefaultStream,
		StreamMaxLen: a.StreamMaxLen,
		LeaseTTL:     time.Duration(a.LeaseTTLMs) * time.Millisecond,
	}
	if a.SigningKey != "" {
		key, err := price.LoadSigningKey(string(a.SigningKey))
		if err != nil {
			_ = redisService.Close()
			return nil, nil, err
		}
		opts.Signer = price.NewSigner(key)
		log.Printf("Prices are signed with the key %s", opts.Signer.ID())
	} else {
		log.Println("WARNING: price_aggregator.signing_key is not set: the prices in Redis are unsigned, and servers that verify them refuse them")
	}
	if a.PublishToRabbitMQ {
		exchange := config.Config.RabbitMQ.Exchanges.PriceUpdates
		broker, err := rabbitmq.NewRabbitMQService(&config.Config, exchange)
		if err != nil {
			_ = redisService.Close()
			return nil, nil, fmt.Errorf("RabbitMQ: %w", err)
		}
		closers = append(closers, func() { _ = broker.Close() })
		opts.Broker, opts.Exchange = broker, exchange
	}
	return price.NewPublisher(opts), func() {
		for _, c := range closers {
			c()
		}
	}, nil
}

// serve answers the probes until ctx is done or the server fails.
func serve(ctx context.Context, srv *http.Server, p *probes) error {
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	var failure error
	select {
	case <-ctx.Done():
		log.Println("Stopping the price aggregator...")
	case err := <-served:
		failure = fmt.Errorf("the probe server stopped: %w", err)
	}
	p.draining.Store(true)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		failure = errors.Join(failure, err)
	}
	return failure
}

// probes answers the probes, the metrics, and the operators' view of the
// prices and the providers. The address is internal: it must not be
// reachable from the public network.
type probes struct {
	service  *price.Service
	draining atomic.Bool
}

func (p *probes) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /system/live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /system/ready", func(w http.ResponseWriter, r *http.Request) {
		if p.draining.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /prices", func(w http.ResponseWriter, r *http.Request) {
		prices := []price.Price{}
		if p.service != nil && p.service.Prices() != nil {
			prices = p.service.Prices()
		}
		writeJSON(w, http.StatusOK, map[string]any{"prices": prices})
	})
	mux.HandleFunc("GET /providers", func(w http.ResponseWriter, r *http.Request) {
		health := []price.Health{}
		if p.service != nil {
			health = p.service.Health()
		}
		writeJSON(w, http.StatusOK, map[string]any{"providers": health})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// configFlagSet reports whether -config was given: a named file that does
// not exist stops the aggregator instead of falling back to the example
// (#306).
func configFlagSet() (set bool) {
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == "config" })
	return set
}
