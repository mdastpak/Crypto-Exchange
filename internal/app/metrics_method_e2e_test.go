package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/httpmethod"
	"github.com/mdastpak/crypto-exchange/internal/metrics"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMetrics_AMadeUpMethodAddsNoSeries sends requests with methods that a
// client made up through the router (#403), past the REST limit, so that
// most are rejected with 429 after the metrics recorded them. Every method
// label and every span name keeps to the standard methods and "other".
func TestMetrics_AMadeUpMethodAddsNoSeries(t *testing.T) {
	env := newE2EEnv(t)
	limits := &config.Config.RateLimits
	limits.REST.RequestsPerSecond, limits.REST.Burst = 1, 5
	router, err := newEngine()
	require.NoError(t, err)
	routeForTest(t, router, env.handlers)
	spans := recordSpans(t)

	rejected := 0
	for i := range 200 {
		req := httptest.NewRequest(fmt.Sprintf("X%d", i), "/api/v1/public/currencies", nil)
		req.RemoteAddr = "198.51.100.40:40000"
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			rejected++
		}
	}
	require.Positive(t, rejected, "the REST limit rejects some of them")

	families, err := metrics.Registry.Gather()
	require.NoError(t, err)
	labels := 0
	for _, f := range families {
		if f.GetName() != "http_requests_total" && f.GetName() != "http_request_duration_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "method" {
					labels++
					assert.Equal(t, httpmethod.Normalize(l.GetValue()), l.GetValue(), "a method label is a standard method or other")
				}
			}
		}
	}
	require.Positive(t, labels)

	named := 0
	for _, s := range spans.GetSpans() {
		method, _, ok := strings.Cut(s.Name, " ")
		if !ok || !strings.HasPrefix(s.Name, method+" /") && !strings.HasSuffix(s.Name, " unmatched") {
			continue
		}
		named++
		assert.Equal(t, httpmethod.Normalize(method), method, "span %q", s.Name)
	}
	assert.GreaterOrEqual(t, named, 200, "every request has a span")
}
