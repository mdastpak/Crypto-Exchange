package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingJob struct{ runs atomic.Int32 }

func (j *countingJob) Name() string      { return "catalog:test" }
func (j *countingJob) Sources() []string { return nil }
func (j *countingJob) Run(ctx context.Context, run *jobs.Run) error {
	if err := run.Fence(ctx); err != nil {
		return err
	}
	j.runs.Add(1)
	return nil
}

func TestStartJobs_RunsTheJobsUntilALockdownPausesThem(t *testing.T) {
	conn := pgtest.Migrated(t, "testdb_market_worker")
	saved := config.Config
	t.Cleanup(func() { config.Config = saved })
	config.Config.Platform.PollIntervalMs = 100
	config.Config.MarketData = config.MarketDataConfig{Mode: config.MarketDataModeLive,
		PollSeconds: 1, LeaseSeconds: 10, MaxConcurrentJobs: 1, RetryBaseSeconds: 1}

	// A lockdown of market_data is active before the worker starts.
	_, err := conn.Exec(`INSERT INTO platform_controls (hk, kind, feature, starts_at, announce_at, reason)
		VALUES (gen_random_uuid(), 'lockdown', 'market_data', NOW() - interval '1 minute', NOW() - interval '1 minute', 'test')`)
	require.NoError(t, err)

	job := &countingJob{}
	stop, err := startJobs(conn, "market-worker/test", []jobs.Spec{{Job: job, Interval: time.Hour, Timeout: time.Minute}}, nil)
	require.NoError(t, err)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, stop(ctx))
	}()

	time.Sleep(1500 * time.Millisecond)
	assert.Equal(t, int32(0), job.runs.Load(), "the lockdown pauses the job")

	// As platform.Lift does, the change raises the version that the
	// watchers poll.
	_, err = conn.Exec(`UPDATE platform_controls SET lifted_at = NOW() WHERE feature = 'market_data'`)
	require.NoError(t, err)
	_, err = conn.Exec(`UPDATE platform_state SET version = version + 1`)
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return job.runs.Load() == 1 }, 5*time.Second, 50*time.Millisecond,
		"the job runs once the lockdown is lifted")
}
