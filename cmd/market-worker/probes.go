package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/metrics"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// probeTimeout bounds the check of PostgreSQL in the readiness probe.
const probeTimeout = 2 * time.Second

// probes answers the health probes and the metrics of the worker. Liveness answers 200 while
// the process runs. Readiness answers 200 while PostgreSQL answers, and 503
// once the worker stops or while PostgreSQL does not, so that an
// orchestrator can tell a worker that cannot do its work.
type probes struct {
	ping     func(ctx context.Context) error
	draining atomic.Bool
	// stopJobs stops the jobs at shutdown; nil when they do not run.
	stopJobs func(ctx context.Context) error
}

func (p *probes) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /system/live", func(w http.ResponseWriter, r *http.Request) {
		writeStatus(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("GET /system/ready", func(w http.ResponseWriter, r *http.Request) {
		if p.draining.Load() {
			writeStatus(w, http.StatusServiceUnavailable, "draining")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		if err := p.ping(ctx); err != nil {
			log.Printf("Readiness: PostgreSQL does not answer: %v", err)
			writeStatus(w, http.StatusServiceUnavailable, "postgres_unavailable")
			return
		}
		writeStatus(w, http.StatusOK, "ready")
	})
	// The counters that the jobs record in this process, such as
	// row_seal_findings_total, and the metrics of the jobs (#545).
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	return mux
}

func writeStatus(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
}
