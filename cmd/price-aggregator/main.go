// Command price-aggregator is the price engine of the exchange (#58): it
// follows the books and trades of the configured providers, and computes a
// reference price per market every price_aggregator.aggregation_interval_ms.
// See docs/price-engine.md.
//
// It runs apart from the server, so that it can run only on the nodes that
// reach the providers, and so that a slow or blocked provider never touches
// the trading API. It needs no database: several replicas may run at once,
// each following the providers and computing the same prices on its own.
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
	"github.com/mdastpak/crypto-exchange/internal/logging"
	"github.com/mdastpak/crypto-exchange/internal/metrics"
	"github.com/mdastpak/crypto-exchange/internal/price"
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
		p.service = price.NewService(agg, feeds, interval)
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
