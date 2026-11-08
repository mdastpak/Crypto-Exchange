package jobs

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/testutil/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeJob counts its runs and does what run says.
type fakeJob struct {
	name    string
	sources []string
	runs    atomic.Int32
	run     func(ctx context.Context, r *Run) error
}

func (j *fakeJob) Name() string      { return j.name }
func (j *fakeJob) Sources() []string { return j.sources }
func (j *fakeJob) Run(ctx context.Context, r *Run) error {
	j.runs.Add(1)
	if j.run != nil {
		return j.run(ctx, r)
	}
	r.AddRead(3)
	r.AddWritten(2)
	return nil
}

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	return pgtest.Migrated(t, "testdb_marketdata_jobs")
}

func options(node string) Options {
	return Options{Node: node, Mode: marketdata.ModeLive, Poll: time.Hour, Lease: 30 * time.Second,
		MaxConcurrent: 4, RetryBase: time.Minute, Logger: log.New(io.Discard, "", 0)}
}

func newRunner(t *testing.T, db *sql.DB, opts Options, specs ...Spec) *Runner {
	t.Helper()
	r, err := NewRunner(db, opts, specs...)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.Stop(ctx)
	})
	return r
}

// tickAndWait runs one tick and waits for the runs it started.
func tickAndWait(r *Runner) {
	r.tick()
	r.runs.Wait()
}

func spec(job Job) Spec { return Spec{Job: job, Interval: time.Hour, Timeout: time.Minute} }

type leaseRow struct {
	holder   string
	token    int64
	failures int
	nextIn   time.Duration
	held     bool
}

func leaseOf(t *testing.T, db *sql.DB, job string) leaseRow {
	t.Helper()
	var l leaseRow
	var nextIn float64
	require.NoError(t, db.QueryRow(`
		SELECT holder, token, failures, EXTRACT(EPOCH FROM next_run_at - NOW()), lease_until > NOW()
		FROM market_job_leases WHERE job = $1`, job).Scan(&l.holder, &l.token, &l.failures, &nextIn, &l.held))
	l.nextIn = time.Duration(nextIn * float64(time.Second))
	return l
}

func TestRunner_EachDueRunHappensOnceAcrossWorkers(t *testing.T) {
	db := newDB(t)
	job := &fakeJob{name: "catalog", run: func(ctx context.Context, r *Run) error {
		time.Sleep(50 * time.Millisecond) // long enough for the others to try
		return nil
	}}
	runners := []*Runner{
		newRunner(t, db, options("a"), spec(job)),
		newRunner(t, db, options("b"), spec(job)),
		newRunner(t, db, options("c"), spec(job)),
	}
	var wg sync.WaitGroup
	for round := 0; round < 5; round++ {
		for _, r := range runners {
			wg.Add(1)
			go func() { defer wg.Done(); tickAndWait(r) }()
		}
		wg.Wait()
	}
	assert.Equal(t, int32(1), job.runs.Load(), "the job is due again only after its interval")

	l := leaseOf(t, db, "catalog")
	assert.False(t, l.held)
	assert.Equal(t, "", l.holder)
	assert.InDelta(t, time.Hour.Seconds(), l.nextIn.Seconds(), 60)

	runs, err := marketdata.NewStore(db).Runs(context.Background(), "catalog", 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "succeeded", runs[0].Status)
}

func TestRunner_ShardsRunOnDifferentWorkers(t *testing.T) {
	db := newDB(t)
	block := make(chan struct{})
	var nodes sync.Map
	newShard := func(name string) *fakeJob {
		return &fakeJob{name: name, run: func(ctx context.Context, r *Run) error {
			<-block
			return nil
		}}
	}
	shards := []*fakeJob{newShard("tickers:binance"), newShard("tickers:okx")}
	opts := options("a")
	opts.MaxConcurrent = 1
	a := newRunner(t, db, opts, spec(shards[0]), spec(shards[1]))
	opts.Node = "b"
	b := newRunner(t, db, opts, spec(shards[0]), spec(shards[1]))

	a.tick()
	b.tick()
	for _, name := range []string{"tickers:binance", "tickers:okx"} {
		l := leaseOf(t, db, name)
		assert.True(t, l.held, name)
		nodes.Store(l.holder, true)
	}
	_, onA := nodes.Load("a")
	_, onB := nodes.Load("b")
	assert.True(t, onA && onB, "each worker took one shard, within its one slot")
	close(block)
	a.runs.Wait()
	b.runs.Wait()
}

func TestRunner_AnotherWorkerTakesOverAfterTheLeasePasses(t *testing.T) {
	db := newDB(t)
	l := leases{db}
	ctx := context.Background()
	// A worker claimed the job and died: it never renews or releases.
	require.NoError(t, l.ensure(ctx, "catalog"))
	_, ok, err := l.claim(ctx, "catalog", "dead", time.Second)
	require.NoError(t, err)
	require.True(t, ok)

	job := &fakeJob{name: "catalog"}
	r := newRunner(t, db, options("alive"), spec(job))
	tickAndWait(r)
	assert.Equal(t, int32(0), job.runs.Load(), "the lease still holds")

	time.Sleep(1100 * time.Millisecond)
	tickAndWait(r)
	assert.Equal(t, int32(1), job.runs.Load(), "the lease passed, so the job runs here")
	assert.Equal(t, int64(2), leaseOf(t, db, "catalog").token)
}

func TestRunner_AWorkerThatLostItsLeaseStopsWriting(t *testing.T) {
	db := newDB(t)
	opts := options("slow")
	opts.Lease = 3 * time.Second
	var fenceErr error
	took := make(chan struct{})
	job := &fakeJob{name: "catalog", run: func(ctx context.Context, r *Run) error {
		require.NoError(t, r.Fence(ctx), "the lease holds at first")
		<-took
		fenceErr = r.Fence(ctx)
		return fenceErr
	}}
	r := newRunner(t, db, opts, spec(job))
	r.tick()

	// The lease passes, as when the worker cannot reach the database to
	// renew it, and another worker takes the job.
	require.Eventually(t, func() bool { return job.runs.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
	_, err := db.Exec(`UPDATE market_job_leases SET lease_until = NOW() - interval '1 second' WHERE job = 'catalog'`)
	require.NoError(t, err)
	token, ok, err := leases{db}.claim(context.Background(), "catalog", "other", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	close(took)
	r.runs.Wait()

	assert.ErrorIs(t, fenceErr, ErrLeaseLost)
	l := leaseOf(t, db, "catalog")
	assert.Equal(t, "other", l.holder, "the stale worker does not release the new holder's lease")
	assert.Equal(t, token, l.token)
	assert.True(t, l.held)
}

func TestRunner_RenewsTheLeaseOfALongRun(t *testing.T) {
	db := newDB(t)
	opts := options("a")
	opts.Lease = 900 * time.Millisecond
	job := &fakeJob{name: "candles:binance", run: func(ctx context.Context, r *Run) error {
		time.Sleep(2 * time.Second) // more than twice the lease
		return r.Fence(ctx)
	}}
	r := newRunner(t, db, opts, spec(job))
	other := newRunner(t, db, options("b"), spec(job))
	r.tick()
	time.Sleep(1500 * time.Millisecond)
	tickAndWait(other)
	r.runs.Wait()
	assert.Equal(t, int32(1), job.runs.Load(), "the renewed lease kept the other worker out")

	runs, err := marketdata.NewStore(db).Runs(context.Background(), "candles:binance", 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "succeeded", runs[0].Status)
}

func TestRunner_PauseStopsNewAndRunningRuns(t *testing.T) {
	db := newDB(t)
	var paused atomic.Bool
	opts := options("a")
	opts.Paused = paused.Load

	var fenceErr error
	started := make(chan struct{})
	resume := make(chan struct{})
	job := &fakeJob{name: "catalog", run: func(ctx context.Context, r *Run) error {
		close(started)
		<-resume
		fenceErr = r.Fence(ctx)
		return fenceErr
	}}
	r := newRunner(t, db, opts, spec(job))

	paused.Store(true)
	tickAndWait(r)
	assert.Equal(t, int32(0), job.runs.Load(), "no run starts while paused")

	paused.Store(false)
	r.tick()
	<-started
	paused.Store(true)
	close(resume)
	r.runs.Wait()
	assert.ErrorIs(t, fenceErr, ErrPaused)
	l := leaseOf(t, db, "catalog")
	assert.False(t, l.held)
	assert.LessOrEqual(t, l.nextIn, time.Duration(0), "a paused run is due again once the pause ends")
	assert.Equal(t, 0, l.failures)
}

func TestRunner_RunsOnlyTheJobsOfReachableSources(t *testing.T) {
	db := newDB(t)
	global := &fakeJob{name: "catalog:coingecko", sources: []string{"coingecko"}}
	iranian := &fakeJob{name: "tickers:nobitex", sources: []string{"nobitex"}}
	opts := options("iran-node")
	opts.Reachable = func(source string) bool { return source == "nobitex" }
	r := newRunner(t, db, opts, spec(global), spec(iranian))
	tickAndWait(r)
	assert.Equal(t, int32(0), global.runs.Load(), "this node does not reach CoinGecko")
	assert.Equal(t, int32(1), iranian.runs.Load())
}

func TestRunner_DryRunIsRecorded(t *testing.T) {
	db := newDB(t)
	var dry bool
	job := &fakeJob{name: "catalog", run: func(ctx context.Context, r *Run) error {
		dry = r.DryRun()
		r.AddRead(5)
		r.AddWritten(5)
		return nil
	}}
	opts := options("a")
	opts.Mode = marketdata.ModeDryRun
	tickAndWait(newRunner(t, db, opts, spec(job)))
	assert.True(t, dry)

	runs, err := marketdata.NewStore(db).Runs(context.Background(), "catalog", 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, marketdata.ModeDryRun, runs[0].Mode)
	assert.Equal(t, 5, runs[0].ItemsRead)
	assert.Equal(t, 5, runs[0].ItemsWritten)
}

func TestRunner_FailuresBackOffAndASuccessResets(t *testing.T) {
	db := newDB(t)
	fail := true
	job := &fakeJob{name: "catalog", run: func(ctx context.Context, r *Run) error {
		if fail {
			return errors.New("source answered 503")
		}
		return nil
	}}
	r := newRunner(t, db, options("a"), spec(job))
	due := func() {
		_, err := db.Exec(`UPDATE market_job_leases SET next_run_at = NOW() WHERE job = 'catalog'`)
		require.NoError(t, err)
	}

	tickAndWait(r)
	l := leaseOf(t, db, "catalog")
	assert.Equal(t, 1, l.failures)
	// The first wait is the retry base, ±20 %.
	assert.InDelta(t, time.Minute.Seconds(), l.nextIn.Seconds(), 0.2*60+1)

	due()
	tickAndWait(r)
	l = leaseOf(t, db, "catalog")
	assert.Equal(t, 2, l.failures)
	assert.InDelta(t, 2*time.Minute.Seconds(), l.nextIn.Seconds(), 0.2*2*60+1)

	for range 6 {
		due()
		tickAndWait(r)
	}
	l = leaseOf(t, db, "catalog")
	assert.LessOrEqual(t, l.nextIn, time.Duration(1.2*float64(time.Hour)), "the wait never passes the interval, with its jitter")

	fail = false
	due()
	tickAndWait(r)
	l = leaseOf(t, db, "catalog")
	assert.Equal(t, 0, l.failures)

	runs, err := marketdata.NewStore(db).Runs(context.Background(), "catalog", 1)
	require.NoError(t, err)
	assert.Equal(t, "succeeded", runs[0].Status)
}

func TestRunner_APanicFailsTheRunOnly(t *testing.T) {
	db := newDB(t)
	job := &fakeJob{name: "catalog", run: func(ctx context.Context, r *Run) error { panic("bug") }}
	tickAndWait(newRunner(t, db, options("a"), spec(job)))
	runs, err := marketdata.NewStore(db).Runs(context.Background(), "catalog", 1)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, "failed", runs[0].Status)
	assert.Contains(t, runs[0].Error, "panicked")
}

func TestRunner_StopWaitsForRunsThenCancelsThem(t *testing.T) {
	db := newDB(t)
	job := &fakeJob{name: "catalog", run: func(ctx context.Context, r *Run) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}}
	r, err := NewRunner(db, options("a"), spec(job))
	require.NoError(t, err)
	r.Start()
	require.Eventually(t, func() bool { return job.runs.Load() == 1 }, 5*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	assert.Error(t, r.Stop(ctx), "the run outlived the deadline and was cancelled")

	l := leaseOf(t, db, "catalog")
	assert.False(t, l.held)
	assert.LessOrEqual(t, l.nextIn, time.Duration(0), "another worker may take the job at once")
	assert.Equal(t, 0, l.failures)
}

func TestNewRunner_RefusesInvalidJobs(t *testing.T) {
	db := newDB(t)
	_, err := NewRunner(db, options("a"), spec(&fakeJob{name: "Catalog Job"}))
	assert.Error(t, err)
	_, err = NewRunner(db, options("a"), spec(&fakeJob{name: "x"}), spec(&fakeJob{name: "x"}))
	assert.Error(t, err)
	_, err = NewRunner(db, options("a"), Spec{Job: &fakeJob{name: "x"}})
	assert.Error(t, err)
	opts := options("a")
	opts.Mode = "shadow"
	_, err = NewRunner(db, opts)
	assert.Error(t, err)
	opts = options("a")
	opts.Lease = 0
	_, err = NewRunner(db, opts)
	assert.Error(t, err)
}
