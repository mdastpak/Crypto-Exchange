package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdastpak/crypto-exchange/internal/httpmethod"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMiddleware_LabelsTheRouteNotThePath records requests by the pattern of
// their route, so that a value of the path never becomes a label.
func TestMiddleware_LabelsTheRouteNotThePath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware())
	r.GET("/items/:id", func(c *gin.Context) { c.Status(http.StatusTeapot) })
	before := testutil.ToFloat64(HTTPRequests.WithLabelValues(http.MethodGet, "/items/:id", "418"))

	for _, id := range []string{"1", "2", "sara@example.com"} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/items/"+id, nil))
	}
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nowhere", nil))

	assert.Equal(t, before+3, testutil.ToFloat64(HTTPRequests.WithLabelValues(http.MethodGet, "/items/:id", "418")))
	assert.Equal(t, 1.0, testutil.ToFloat64(HTTPRequests.WithLabelValues(http.MethodGet, "unmatched", "404")))
	assert.GreaterOrEqual(t, testutil.CollectAndCount(HTTPDuration, "http_request_duration_seconds"), 2,
		"the durations are recorded")
}

// methodsOf returns the values of the method label of the series of
// http_requests_total for route.
func methodsOf(t *testing.T, route string) map[string]bool {
	t.Helper()
	families, err := Registry.Gather()
	require.NoError(t, err)
	methods := map[string]bool{}
	for _, f := range families {
		if f.GetName() != "http_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["route"] == route {
				methods[labels["method"]] = true
			}
		}
	}
	return methods
}

// TestMiddleware_BoundsTheMethodLabel sends many methods that a client
// made up (#403): they share one label value, "other", so that a client
// cannot add series. The router matches no route for them, as for any
// method it has no handler of.
func TestMiddleware_BoundsTheMethodLabel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware())
	r.Any("/cardinality", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	before := testutil.ToFloat64(HTTPRequests.WithLabelValues(httpmethod.Other, "unmatched", "404"))

	for i := range 300 {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(fmt.Sprintf("M%d", i), "/cardinality", nil))
	}
	for _, m := range []string{http.MethodGet, http.MethodPost, "get", "PROPFIND"} {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(m, "/cardinality", nil))
	}

	assert.Equal(t, map[string]bool{"GET": true, "POST": true}, methodsOf(t, "/cardinality"))
	for method := range methodsOf(t, "unmatched") {
		assert.Equal(t, method, httpmethod.Normalize(method), "every method label is a standard method or other")
	}
	assert.Equal(t, before+302, testutil.ToFloat64(HTTPRequests.WithLabelValues(httpmethod.Other, "unmatched", "404")))
}

// TestServer_ServesTheRegistryOnItsOwnAddress scrapes a server of the
// registry, with a gauge, a counter, and the Go runtime.
func TestServer_ServesTheRegistryOnItsOwnAddress(t *testing.T) {
	require.NoError(t, Gauge("test_gauge", "A gauge of the test.", func() float64 { return 7 }))
	assert.Error(t, Gauge("test_gauge", "Again.", func() float64 { return 8 }), "a name is registered once")
	SealFindings.WithLabelValues("users", "mismatch").Inc()
	RateLimitRejections.WithLabelValues("rest").Inc()

	server, err := Listen("127.0.0.1:0", "/metrics")
	require.NoError(t, err)
	server.Start()
	t.Cleanup(func() { _ = server.Stop(context.Background()) })

	resp, err := http.Get("http://" + server.Addr() + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	text := string(body)
	for _, line := range []string{
		"test_gauge 7",
		`row_seal_findings_total{check="mismatch",table="users"}`,
		`rate_limit_rejections_total{limiter="rest"}`,
		"go_goroutines",
	} {
		assert.Contains(t, text, line)
	}
	resp, err = http.Get("http://" + server.Addr() + "/other")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "only the metrics path is served")
}
