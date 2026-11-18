package state

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/venues"
)

// Job takes the snapshots of one venue and reconciles it, as a job of
// cmd/market-worker under a lease (#264), so that one node at a time calls
// the venue for its state. One run, in order: the markets, the networks of
// the mapped assets, the fees that the venue reports (FeeReporter), the
// balances, and then the reconciliation, so that every fee posted is in
// the balances that the reconciliation compares.
type Job struct {
	Store   *Store
	Venue   venues.Venue
	Mapping venues.Mapping
	// Now is the clock of the snapshots; the Store's when nil.
	Now func() time.Time
}

// Name implements jobs.Job.
func (j Job) Name() string { return "venue_state:" + j.Venue.Name() }

// Sources implements jobs.Job: none of the market data sources. A node
// runs the job of a venue only when venues.exchanges enables it there.
func (j Job) Sources() []string { return nil }

// now is the time of a snapshot, read before the venue is called: a move
// that the ledger posts while the venue answers is then newer than the
// snapshot, which the reconciliation skips, rather than older than it.
func (j Job) now(ctx context.Context) time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return j.Store.Clock(ctx)
}

// Run implements jobs.Job. A snapshot that fails is recorded by the kind of
// its error and leaves the last one in place, which goes stale on its own;
// the others are still taken. The run fails when any did.
func (j Job) Run(ctx context.Context, run *jobs.Run) error {
	name := j.Venue.Name()
	var failed []error
	fail := func(kind string, err error) {
		failed = append(failed, err)
		if !run.DryRun() {
			_ = j.Store.RecordFailure(ctx, name, kind, err)
		}
	}

	instrumentsAt := j.now(ctx)
	if instruments, err := j.Venue.Instruments(ctx); err != nil {
		fail(KindInstruments, err)
	} else {
		run.AddRead(len(instruments))
		if err := j.write(ctx, run, len(instruments), func() error {
			return j.Store.SaveInstruments(ctx, name, instruments, instrumentsAt)
		}); err != nil {
			return err
		}
	}

	assets := make([]string, 0, len(j.Mapping))
	for asset := range j.Mapping {
		assets = append(assets, asset)
	}
	sort.Strings(assets)
	networksAt := j.now(ctx)
	networks := map[string][]venues.Network{}
	var networkErr error
	for _, asset := range assets {
		list, err := j.Venue.Networks(ctx, asset)
		if err != nil {
			networkErr = err
			continue
		}
		run.AddRead(len(list))
		networks[asset] = list
	}
	if networkErr != nil {
		fail(KindNetworks, networkErr)
	}
	if len(networks) > 0 {
		if err := j.write(ctx, run, len(networks), func() error {
			return j.Store.SaveNetworks(ctx, name, networks, networksAt)
		}); err != nil {
			return err
		}
	}

	if reporter, ok := j.Venue.(FeeReporter); ok {
		feesAt := j.now(ctx)
		since, err := j.Store.FeesSince(ctx, name, feesAt)
		if err != nil {
			return err
		}
		fees, err := reporter.Fees(ctx, since)
		if err != nil {
			// The balances would show the fees not read as differences.
			fail(KindFees, err)
			return errors.Join(failed...)
		}
		run.AddRead(len(fees))
		if err := j.write(ctx, run, max(len(fees), 1), func() error {
			if _, err := j.Store.PostFees(ctx, name, j.Mapping, fees); err != nil {
				return err
			}
			return j.Store.SaveFeeCursor(ctx, name, feesAt)
		}); err != nil {
			return err
		}
	}

	balancesAt := j.now(ctx)
	balances, err := j.Venue.Balances(ctx)
	if err != nil {
		fail(KindBalances, err)
		return errors.Join(failed...)
	}
	run.AddRead(len(balances))
	if err := j.write(ctx, run, len(balances), func() error {
		return j.Store.SaveBalances(ctx, name, balances, balancesAt)
	}); err != nil {
		return err
	}
	if !run.DryRun() {
		if err := run.Fence(ctx); err != nil {
			return err
		}
		if _, err := j.Store.Reconcile(ctx, name, j.Mapping); err != nil {
			return err
		}
	}
	return errors.Join(failed...)
}

// write writes n items with save, after the fence; a dry run counts them
// only.
func (j Job) write(ctx context.Context, run *jobs.Run, n int, save func() error) error {
	run.AddWritten(n)
	if run.DryRun() {
		return nil
	}
	if err := run.Fence(ctx); err != nil {
		return err
	}
	return save()
}
