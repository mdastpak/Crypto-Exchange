// Package jobs runs the jobs of cmd/market-worker (#264). Every worker runs
// a Runner with the same jobs, and each run of a job happens on one worker:
// a worker claims a due job through its lease in market_job_leases, renews
// the lease while the run goes on, and gives it up with the time of the
// next run. A worker that dies stops renewing, and another worker claims the
// job once the lease has passed.
//
// A run checks its lease with Run.Fence before each batch of writes, and
// stops when another worker took the job or a lockdown of market_data
// paused the jobs. The writes of the store are idempotent and never put an
// older value back, so a batch that a worker writes just as its lease
// passes does no harm.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
)

// ErrLeaseLost stops a run whose lease another worker took, for example
// after this worker could not renew it in time.
var ErrLeaseLost = errors.New("jobs: the lease of the job was lost")

// ErrPaused stops a run when a lockdown pauses the jobs.
var ErrPaused = errors.New("jobs: the jobs are paused")

// errStopping stops the runs when the worker stops.
var errStopping = errors.New("jobs: the worker is stopping")

var jobName = regexp.MustCompile(`^[a-z0-9_.:-]{1,100}$`)

// Job is a job of the worker.
type Job interface {
	// Name names the job, and the shard of its work, such as
	// "tickers:binance". It is the key of the job's lease.
	Name() string
	// Sources are the external sources that the job calls. A worker runs
	// the job only when it reaches all of them.
	Sources() []string
	// Run does one run. It calls run.Fence before each batch of writes,
	// writes nothing when run.DryRun, and counts its items on run.
	Run(ctx context.Context, run *Run) error
}

// Spec schedules a job.
type Spec struct {
	Job Job
	// Interval is the time from the end of a successful run to the next.
	Interval time.Duration
	// Timeout bounds one run.
	Timeout time.Duration
}

// Run is one run of a job, as the job sees it.
type Run struct {
	mode    string
	read    atomic.Int64
	written atomic.Int64
	fence   func(ctx context.Context) error
}

// DryRun reports whether the run must not write: it collects and checks,
// and counts what it would write.
func (r *Run) DryRun() bool { return r.mode == marketdata.ModeDryRun }

// Mode is the mode of the run, marketdata.ModeLive or ModeDryRun.
func (r *Run) Mode() string { return r.mode }

// Fence returns ErrLeaseLost when the worker no longer holds the lease of
// the job, and ErrPaused when a lockdown paused the jobs. The job calls it
// before each batch of writes, and stops on an error.
func (r *Run) Fence(ctx context.Context) error { return r.fence(ctx) }

// AddRead counts items that the run read from a source.
func (r *Run) AddRead(n int) { r.read.Add(int64(n)) }

// AddWritten counts items that the run wrote, or would write in a dry run.
func (r *Run) AddWritten(n int) { r.written.Add(int64(n)) }

// Read and Written are the counts of the run.
func (r *Run) Read() int    { return int(r.read.Load()) }
func (r *Run) Written() int { return int(r.written.Load()) }

// NewRun returns a run outside a Runner, for the tests of a job: in mode,
// with a fence that always holds when fence is nil.
func NewRun(mode string, fence func(ctx context.Context) error) *Run {
	if fence == nil {
		fence = func(context.Context) error { return nil }
	}
	return &Run{mode: mode, fence: fence}
}

// Options configure a Runner.
type Options struct {
	// Node names the worker in the leases and the run records.
	Node string
	// Mode is marketdata.ModeLive or ModeDryRun.
	Mode string
	// Poll is how often the runner looks for due jobs.
	Poll time.Duration
	// Lease is how long a lease lasts without a renewal: how soon another
	// worker takes over the job of a worker that died. It is renewed every
	// third of it.
	Lease time.Duration
	// MaxConcurrent bounds the runs of this worker at once.
	MaxConcurrent int
	// RetryBase is the first wait after a failed run; it doubles with
	// each failure in a row, up to the interval of the job.
	RetryBase time.Duration
	// Reachable reports whether this worker reaches a source. Nil reaches
	// every source.
	Reachable func(source string) bool
	// Paused reports whether a lockdown pauses the jobs. Nil never pauses.
	Paused func() bool
	Logger *log.Logger
}

// Runner runs the jobs of one worker.
type Runner struct {
	leases leases
	store  *marketdata.Store
	opts   Options
	specs  []Spec

	running sync.Map // job name -> struct{}
	slots   chan struct{}
	runs    sync.WaitGroup

	// ctx ends the runs: Stop cancels it once its deadline passes.
	ctx      context.Context
	cancel   context.CancelCauseFunc
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// NewRunner returns a runner of specs on db. It refuses a job without a
// valid, unique name or with a non-positive interval or timeout.
func NewRunner(db *sql.DB, opts Options, specs ...Spec) (*Runner, error) {
	if opts.Mode != marketdata.ModeLive && opts.Mode != marketdata.ModeDryRun {
		return nil, fmt.Errorf("jobs: invalid mode %q", opts.Mode)
	}
	if opts.Poll <= 0 || opts.Lease <= 0 || opts.MaxConcurrent <= 0 || opts.RetryBase <= 0 {
		return nil, errors.New("jobs: poll, lease, max concurrent, and retry base must be positive")
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	seen := map[string]bool{}
	for _, s := range specs {
		name := s.Job.Name()
		if !jobName.MatchString(name) {
			return nil, fmt.Errorf("jobs: invalid job name %q", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("jobs: the job %q is listed twice", name)
		}
		seen[name] = true
		if s.Interval <= 0 || s.Timeout <= 0 {
			return nil, fmt.Errorf("jobs: the job %q needs a positive interval and timeout", name)
		}
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	return &Runner{leases: leases{db}, store: marketdata.NewStore(db), opts: opts, specs: specs,
		slots: make(chan struct{}, opts.MaxConcurrent), ctx: ctx, cancel: cancel,
		stop: make(chan struct{}), done: make(chan struct{})}, nil
}

// Start looks for due jobs every poll interval until Stop.
func (r *Runner) Start() {
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(r.opts.Poll)
		defer ticker.Stop()
		for {
			r.tick()
			select {
			case <-r.stop:
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop stops looking for jobs and waits for the runs in progress until ctx
// ends; then it cancels them, and their jobs are due at once for another
// worker.
func (r *Runner) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() { close(r.stop) })
	select {
	case <-r.done:
	case <-ctx.Done():
	}
	finished := make(chan struct{})
	go func() { r.runs.Wait(); close(finished) }()
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		r.cancel(errStopping)
		<-finished
		return fmt.Errorf("jobs: runs cancelled at shutdown: %w", ctx.Err())
	}
}

// tick claims the due jobs that this worker can run, in a random order so
// that the workers spread the jobs between them, and starts their runs.
func (r *Runner) tick() {
	if r.paused() {
		return
	}
	order := rand.Perm(len(r.specs)) //nolint:gosec // G404: the order only spreads the jobs between workers; nothing secret depends on it.
	for _, i := range order {
		spec := r.specs[i]
		name := spec.Job.Name()
		if !r.reaches(spec.Job) {
			continue
		}
		if _, busy := r.running.Load(name); busy {
			continue
		}
		select {
		case r.slots <- struct{}{}:
		default:
			return // every slot is taken
		}
		token, ok := r.claim(name)
		if !ok {
			<-r.slots
			continue
		}
		r.running.Store(name, struct{}{})
		r.runs.Add(1)
		go func() {
			defer r.runs.Done()
			defer func() { <-r.slots }()
			defer r.running.Delete(name)
			r.execute(spec, token)
		}()
	}
}

func (r *Runner) claim(name string) (int64, bool) {
	ctx, cancel := context.WithTimeout(r.ctx, r.opts.Lease/2)
	defer cancel()
	if err := r.leases.ensure(ctx, name); err != nil {
		r.opts.Logger.Printf("market data job %s: adding its lease: %v", name, err)
		return 0, false
	}
	token, ok, err := r.leases.claim(ctx, name, r.opts.Node, r.opts.Lease)
	if err != nil {
		r.opts.Logger.Printf("market data job %s: claiming its lease: %v", name, err)
		return 0, false
	}
	return token, ok
}

// execute runs a claimed job, renews its lease while it runs, records the
// run, and releases the lease.
func (r *Runner) execute(spec Spec, token int64) {
	name := spec.Job.Name()
	ctx, cancel := context.WithCancelCause(r.ctx)
	defer cancel(nil)
	ctx, cancelTimeout := context.WithTimeout(ctx, spec.Timeout)
	defer cancelTimeout()

	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		r.renew(ctx, name, token, cancel)
	}()

	run := &Run{mode: r.opts.Mode}
	run.fence = func(ctx context.Context) error {
		if r.paused() {
			return ErrPaused
		}
		if err := context.Cause(ctx); err != nil {
			return err
		}
		ok, err := r.leases.holds(ctx, name, token)
		if err != nil {
			return fmt.Errorf("jobs: checking the lease: %w", err)
		}
		if !ok {
			return ErrLeaseLost
		}
		return nil
	}

	started := time.Now()
	record := context.WithoutCancel(ctx)
	runID, err := r.store.StartRun(record, name, r.opts.Node, r.opts.Mode)
	if err != nil {
		r.opts.Logger.Printf("market data job %s: recording its start: %v", name, err)
	}
	runErr := r.call(ctx, spec.Job, run)
	if cause := context.Cause(ctx); runErr != nil && cause != nil && !errors.Is(runErr, cause) {
		runErr = fmt.Errorf("%w: %w", cause, runErr)
	}
	cancel(nil)
	<-renewed

	read, written := int(run.read.Load()), int(run.written.Load())
	if runID != 0 {
		if err := r.store.FinishRun(record, runID, read, written, runErr); err != nil {
			r.opts.Logger.Printf("market data job %s: recording its end: %v", name, err)
		}
	}

	how := succeeded
	switch {
	case runErr == nil:
	case errors.Is(runErr, errStopping), errors.Is(runErr, ErrPaused):
		how = interrupted
	default:
		how = failed
	}
	releaseCtx, cancelRelease := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelRelease()
	if err := r.leases.release(releaseCtx, name, token, how, spec.Interval, r.opts.RetryBase); err != nil {
		r.opts.Logger.Printf("market data job %s: releasing its lease; it is due again once the lease passes: %v", name, err)
	}

	took := time.Since(started).Round(time.Millisecond)
	if runErr != nil {
		r.opts.Logger.Printf("market data job %s (%s) failed after %s, %d read, %d written: %v", name, r.opts.Mode, took, read, written, runErr)
		return
	}
	r.opts.Logger.Printf("market data job %s (%s) finished in %s, %d read, %d written", name, r.opts.Mode, took, read, written)
}

// call runs the job and turns a panic into an error, so that one faulty
// job cannot stop the worker.
func (r *Runner) call(ctx context.Context, job Job, run *Run) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("jobs: the job panicked: %v", p)
		}
	}()
	return job.Run(ctx, run)
}

// renew renews the lease every third of its time until ctx ends, and
// cancels the run with ErrLeaseLost when the lease is lost. A renewal that
// fails for a moment is tried again at the next third; the lease lasts
// until then.
func (r *Runner) renew(ctx context.Context, name string, token int64, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(r.opts.Lease / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		renewCtx, cancelRenew := context.WithTimeout(ctx, r.opts.Lease/3)
		ok, err := r.leases.renew(renewCtx, name, token, r.opts.Lease)
		cancelRenew()
		switch {
		case err != nil && ctx.Err() == nil:
			r.opts.Logger.Printf("market data job %s: renewing its lease: %v", name, err)
		case err == nil && !ok:
			cancel(ErrLeaseLost)
			return
		}
	}
}

func (r *Runner) paused() bool { return r.opts.Paused != nil && r.opts.Paused() }

func (r *Runner) reaches(job Job) bool {
	if r.opts.Reachable == nil {
		return true
	}
	for _, source := range job.Sources() {
		if !r.opts.Reachable(source) {
			return false
		}
	}
	return true
}
