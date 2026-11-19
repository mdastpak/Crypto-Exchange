package venues

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
	"github.com/mdastpak/crypto-exchange/internal/metrics"
)

// Every call is counted by venue, call, and outcome, and timed (#545).
func TestCaller_RecordsEachCallOnTheMetrics(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	client, err := source.NewClient(source.ClientOptions{Source: "venue_metrics", BaseURL: srv.URL, Timeout: time.Second, MaxAttempts: 1})
	require.NoError(t, err)
	c := Caller{Venue: "metrics", Client: client, Classify: func(int, []byte) (Kind, string) { return "", "" }}
	calls := func(outcome string) float64 {
		return testutil.ToFloat64(metrics.VenueCalls.WithLabelValues("metrics", "balances", outcome))
	}
	read := source.Request{Method: http.MethodGet, Path: "x", Idempotent: true}

	require.NoError(t, c.Call(context.Background(), "balances", read, nil))
	status = http.StatusBadRequest
	require.Error(t, c.Call(context.Background(), "balances", read, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, c.Call(ctx, "balances", read, nil))

	assert.Equal(t, 1.0, calls("ok"))
	assert.Equal(t, 1.0, calls(string(KindRejected)))
	assert.Equal(t, 1.0, calls("canceled"))
	var m dto.Metric
	require.NoError(t, metrics.VenueCallDuration.WithLabelValues("metrics", "balances").(prometheus.Histogram).Write(&m))
	assert.Equal(t, uint64(3), m.GetHistogram().GetSampleCount())
}
