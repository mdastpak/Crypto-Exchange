package system

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

var dependencyNames = []string{"postgres", "redis", "rabbitmq"}

// handlerFailing returns a handler whose named dependencies fail.
func handlerFailing(failing ...string) *SystemHandler {
	h := &SystemHandler{}
	for _, name := range dependencyNames {
		var err error
		for _, f := range failing {
			if f == name {
				err = errors.New(name + " is down")
			}
		}
		h.checks = append(h.checks, dependency{name, func(context.Context) error { return err }})
	}
	return h
}

func get(h *SystemHandler, path string) *httptest.ResponseRecorder {
	router := gin.New()
	router.GET("/system/live", h.LivenessHandler)
	router.GET("/system/ready", h.ReadinessHandler)
	router.GET("/system/healthcheck", h.HealthCheckHandler)
	router.GET("/system/ping", h.PingPongHandler)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

func healthOf(t *testing.T, w *httptest.ResponseRecorder) HealthStatus {
	t.Helper()
	var status HealthStatus
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &status), w.Body.String())
	return status
}

func TestReadiness_AllDependenciesAvailable(t *testing.T) {
	w := get(handlerFailing(), "/system/ready")
	assert.Equal(t, http.StatusOK, w.Code)
	status := healthOf(t, w)
	assert.Equal(t, "ok", status.Status)
	for _, name := range dependencyNames {
		assert.Equal(t, "ok", status.Details[name].Status, name)
		assert.False(t, status.Details[name].LastCheck.IsZero(), name)
	}
}

func TestReadiness_EachDependencyFailureMakesTheInstanceUnavailable(t *testing.T) {
	for _, failing := range dependencyNames {
		w := get(handlerFailing(failing), "/system/ready")
		assert.Equal(t, http.StatusServiceUnavailable, w.Code, failing)
		status := healthOf(t, w)
		assert.Equal(t, "degraded", status.Status, failing)
		for _, name := range dependencyNames {
			want := "ok"
			if name == failing {
				want = "error"
			}
			assert.Equal(t, want, status.Details[name].Status, "%s down: %s", failing, name)
		}
	}
}

func TestReadiness_ServicesThatAreNotConfiguredAreUnavailable(t *testing.T) {
	w := get(&SystemHandler{}, "/system/ready")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	status := healthOf(t, w)
	for _, name := range dependencyNames {
		assert.Equal(t, "error", status.Details[name].Status, name)
	}
}

func TestReadiness_ASlowDependencyIsReportedWithinTheTimeout(t *testing.T) {
	h := handlerFailing()
	h.checks[0] = dependency{"postgres", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	h.checkTimeout = 100 * time.Millisecond

	start := time.Now()
	w := get(h, "/system/ready")
	assert.Less(t, time.Since(start), time.Second, "the checks run at once, each with a timeout")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "error", healthOf(t, w).Details["postgres"].Status)
}

func TestReadiness_HidesTheDetailsInReleaseMode(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	t.Cleanup(func() { gin.SetMode(gin.TestMode) })

	w := get(handlerFailing("redis"), "/system/ready")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.JSONEq(t, `{"status": "degraded"}`, w.Body.String())
}

func TestHealthCheck_AnswersLikeReadiness(t *testing.T) {
	assert.Equal(t, http.StatusOK, get(handlerFailing(), "/system/healthcheck").Code)
	w := get(handlerFailing("postgres"), "/system/healthcheck")
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "error", healthOf(t, w).Details["postgres"].Status)
}

func TestLiveness_DoesNotDependOnTheDependencies(t *testing.T) {
	w := get(handlerFailing(dependencyNames...), "/system/live")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"status": "ok"}`, w.Body.String())
}

func TestPingPongHandler(t *testing.T) {
	w := get(&SystemHandler{}, "/system/ping")
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "pong", body["message"])
	_, err := time.Parse(time.RFC3339, body["timestamp"])
	assert.NoError(t, err)
}
