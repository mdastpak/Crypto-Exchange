package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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
