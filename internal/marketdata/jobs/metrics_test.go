package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"

	"github.com/mdastpak/crypto-exchange/internal/metrics"
)

// Each run is counted by its outcome, a success sets the time of the last
// one, and the interval of each job is exported, so that an alert can
// tell a job that stopped running (#545).
func TestRunner_RecordsItsRunsOnTheMetrics(t *testing.T) {
	db := newDB(t)
	fail := true
	job := &fakeJob{name: "metrics_job", run: func(context.Context, *Run) error {
		if fail {
			return errors.New("source answered 503")
		}
		return nil
	}}
	r := newRunner(t, db, options("a"), Spec{Job: job, Interval: 90 * time.Second, Timeout: time.Minute})
	runs := func(outcome string) float64 {
		return testutil.ToFloat64(metrics.MarketJobRuns.WithLabelValues("metrics_job", outcome))
	}
	failedBefore, succeededBefore := runs("failed"), runs("succeeded")

	tickAndWait(r)
	assert.Equal(t, failedBefore+1, runs("failed"))
	assert.Zero(t, testutil.ToFloat64(metrics.MarketJobLastSuccess.WithLabelValues("metrics_job")),
		"a failed run is no success")

	fail = false
	_, err := db.Exec(`UPDATE market_job_leases SET next_run_at = NOW() WHERE job = 'metrics_job'`)
	assert.NoError(t, err)
	tickAndWait(r)
	assert.Equal(t, succeededBefore+1, runs("succeeded"))
	assert.InDelta(t, float64(time.Now().Unix()),
		testutil.ToFloat64(metrics.MarketJobLastSuccess.WithLabelValues("metrics_job")), 5)
	var m dto.Metric
	assert.NoError(t, metrics.MarketJobDuration.WithLabelValues("metrics_job").(prometheus.Histogram).Write(&m))
	assert.Equal(t, uint64(2), m.GetHistogram().GetSampleCount(), "every run is timed, whatever its outcome")
	assert.Equal(t, 90.0, testutil.ToFloat64(metrics.MarketJobInterval.WithLabelValues("metrics_job")))
}
