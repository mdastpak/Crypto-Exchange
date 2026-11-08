// Package catalog is the job that keeps the catalog of assets from a
// catalog source (#266): one job per source, "catalog:coingecko" and
// "catalog:coinmarketcap", which every worker runs and one runs at a time.
//
// A run reads the source's listing, which gives symbols, names, ranks, and
// supplies in bulk, and matches each asset to the catalog: by its identity
// at the source, then by the contracts of a token. A symbol alone never
// merges two assets. It then reads a bounded number of full records
// (description, links, contracts), the missing and the oldest first, so
// that a large catalog fills over several runs within the source's limits.
//
// The sources have a priority. The primary source (CoinGecko by default)
// writes the fields of the assets it knows and creates any asset it lists.
// A secondary source writes the fields of the assets that only it knows,
// links its identities to the catalog's assets, and creates an asset only
// when nothing in the catalog may be the same asset. What it cannot decide
// is recorded as a conflict for an administrator.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/jobs"
	"github.com/mdastpak/crypto-exchange/internal/marketdata/source"
)

// Coverage says which assets of a source the catalog keeps.
const (
	// CoverageList keeps a fixed list of ids: the development list, which
	// gives useful test cases with few requests.
	CoverageList = "list"
	// CoverageTop keeps the first Top assets by market cap.
	CoverageTop = "top"
	// CoverageAll keeps every asset of the source.
	CoverageAll = "all"
)

// batchSize is how many assets a run writes between two checks of its
// lease.
const batchSize = 50

// Options configure the job of one source.
type Options struct {
	// Priority lists the catalog sources, primary first.
	Priority []string
	Coverage string
	// Top is the number of assets of CoverageTop.
	Top int
	// IDs are the ids of CoverageList.
	IDs []string
	// DetailsPerRun bounds the full records read in one run.
	DetailsPerRun int
	// DetailsMaxAge is when a full record is read again.
	DetailsMaxAge time.Duration
	// DeactivateAfter marks inactive the assets that no source listed for
	// this long. It applies in the runs of the primary source with
	// CoverageAll only, since only those list every asset.
	DeactivateAfter time.Duration
	Now             func() time.Time
}

// RecordReader is a source that reads several full records in one request,
// such as CoinMarketCap.
type RecordReader interface {
	Records(ctx context.Context, ids []string) (map[string]source.Asset, error)
}

// Job keeps the catalog from one source.
type Job struct {
	src   source.CatalogSource
	store *marketdata.Store
	opts  Options
}

// New returns the job of src.
func New(src source.CatalogSource, store *marketdata.Store, opts Options) (*Job, error) {
	switch opts.Coverage {
	case CoverageList:
		if len(opts.IDs) == 0 {
			return nil, fmt.Errorf("catalog %s: the list coverage needs ids", src.Name())
		}
	case CoverageTop:
		if opts.Top <= 0 {
			return nil, fmt.Errorf("catalog %s: the top coverage needs a positive number", src.Name())
		}
	case CoverageAll:
	default:
		return nil, fmt.Errorf("catalog %s: unknown coverage %q", src.Name(), opts.Coverage)
	}
	if !slices.Contains(opts.Priority, src.Name()) {
		return nil, fmt.Errorf("catalog %s: the source is not in the priority list", src.Name())
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Job{src: src, store: store, opts: opts}, nil
}

// Name implements jobs.Job.
func (j *Job) Name() string { return "catalog:" + j.src.Name() }

// Sources implements jobs.Job.
func (j *Job) Sources() []string { return []string{j.src.Name()} }

func (j *Job) primary() bool { return j.opts.Priority[0] == j.src.Name() }

// Run implements jobs.Job.
func (j *Job) Run(ctx context.Context, run *jobs.Run) error {
	now := j.opts.Now().UTC()
	listing, err := j.src.Assets(ctx, j.listOptions())
	if err != nil {
		return fmt.Errorf("listing: %w", err)
	}
	run.AddRead(len(listing))

	seen := make([]int64, 0, len(listing))
	for i, a := range listing {
		if i%batchSize == 0 {
			if err := run.Fence(ctx); err != nil {
				return err
			}
		}
		id, err := j.applyListing(ctx, run, a)
		if err != nil {
			return fmt.Errorf("asset %s: %w", a.ExternalID, err)
		}
		if id != 0 {
			seen = append(seen, id)
		}
	}
	if !run.DryRun() {
		if err := run.Fence(ctx); err != nil {
			return err
		}
		if err := j.store.MarkAssetsSeen(ctx, seen, now); err != nil {
			return err
		}
	}

	if err := j.readRecords(ctx, run, now); err != nil {
		return err
	}

	if j.primary() && j.opts.Coverage == CoverageAll && j.opts.DeactivateAfter > 0 && !run.DryRun() {
		if err := run.Fence(ctx); err != nil {
			return err
		}
		n, err := j.store.DeactivateUnseen(ctx, now.Add(-j.opts.DeactivateAfter))
		if err != nil {
			return err
		}
		run.AddWritten(int(n))
	}
	return nil
}

func (j *Job) listOptions() source.ListOptions {
	switch j.opts.Coverage {
	case CoverageList:
		return source.ListOptions{IDs: j.opts.IDs}
	case CoverageTop:
		return source.ListOptions{Limit: j.opts.Top}
	}
	return source.ListOptions{}
}

// applyListing matches an asset of the listing to the catalog, creating or
// linking it as the rules allow, and writes its listing fields where this
// source owns them. It returns the asset's id, or 0 when the asset was left
// for an administrator or, in a dry run, would be created.
func (j *Job) applyListing(ctx context.Context, run *jobs.Run, a source.Asset) (int64, error) {
	id, created, err := j.resolve(ctx, run, a)
	if err != nil || id == 0 || created {
		return id, err
	}
	owns, err := j.owns(ctx, id)
	if err != nil || !owns {
		return id, err
	}
	if run.DryRun() {
		return id, nil
	}
	d := a.Details
	changed, err := j.store.UpdateListing(ctx, id, marketdata.Listing{
		Symbol: d.Symbol, Name: d.Name, LogoURL: d.LogoURL, MarketCapRank: d.MarketCapRank,
		CirculatingSupply: d.CirculatingSupply, TotalSupply: d.TotalSupply, MaxSupply: d.MaxSupply,
		Attribution: d.Attribution,
	})
	if changed {
		run.AddWritten(1)
	}
	return id, err
}

// owns reports whether this source writes the fields of the asset: it is
// the source of highest priority among those that know the asset.
func (j *Job) owns(ctx context.Context, id int64) (bool, error) {
	sources, err := j.store.SourcesOf(ctx, id)
	if err != nil {
		return false, err
	}
	for _, name := range j.opts.Priority {
		if slices.Contains(sources, name) {
			return name == j.src.Name(), nil
		}
	}
	return false, nil
}

// resolve finds the catalog's asset of a, links this source's identity to
// it, or creates it. created reports a new asset, whose fields are already
// written.
func (j *Job) resolve(ctx context.Context, run *jobs.Run, a source.Asset) (id int64, created bool, err error) {
	name := j.src.Name()
	id, err = j.store.AssetBySource(ctx, name, a.ExternalID)
	if err == nil {
		return id, false, nil
	}
	if !errors.Is(err, marketdata.ErrNotFound) {
		return 0, false, err
	}

	// A token whose contract the catalog knows is that asset.
	owners := map[int64]bool{}
	for _, c := range a.Contracts {
		owner, err := j.store.AssetByContract(ctx, c.Network, c.Address)
		if errors.Is(err, marketdata.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, false, err
		}
		owners[owner] = true
	}
	switch len(owners) {
	case 0:
	case 1:
		for owner := range owners {
			return j.link(ctx, run, owner, a)
		}
	default:
		return 0, false, j.conflict(ctx, run, marketdata.Conflict{Kind: marketdata.ConflictIdentity, ExternalID: a.ExternalID,
			Detail: fmt.Sprintf("the contracts of %s belong to %d different assets", a.ExternalID, len(owners))})
	}

	// A coin of the same symbol and name, which no catalog source knows
	// under another id at this source, is the same coin: Bitcoin is
	// Bitcoin at every source. A token is matched by contract only.
	all, sameName, err := j.store.AssetsBySymbol(ctx, a.Details.Symbol, marketdata.Slugify(a.Details.Name))
	if err != nil {
		return 0, false, err
	}
	if len(a.Contracts) == 0 && len(sameName) == 1 {
		sources, err := j.store.SourcesOf(ctx, sameName[0])
		if err != nil {
			return 0, false, err
		}
		if !slices.Contains(sources, name) {
			return j.link(ctx, run, sameName[0], a)
		}
	}
	if !j.primary() && len(all) > 0 {
		// A secondary source creates no asset that may already be in the
		// catalog under another source.
		first := all[0]
		return 0, false, j.conflict(ctx, run, marketdata.Conflict{Kind: marketdata.ConflictSymbol, ExternalID: a.ExternalID,
			Subject: a.Details.Symbol, AssetID: &first,
			Detail: fmt.Sprintf("%s (%s) may be one of %d assets with this symbol", a.Details.Name, a.Details.Symbol, len(all))})
	}
	return j.create(ctx, run, a)
}

func (j *Job) link(ctx context.Context, run *jobs.Run, id int64, a source.Asset) (int64, bool, error) {
	run.AddWritten(1)
	if run.DryRun() {
		return id, false, nil
	}
	owner, err := j.store.LinkSource(ctx, id, j.src.Name(), a.ExternalID)
	if err != nil {
		return 0, false, err
	}
	return owner, false, nil
}

func (j *Job) create(ctx context.Context, run *jobs.Run, a source.Asset) (int64, bool, error) {
	run.AddWritten(1)
	if run.DryRun() {
		return 0, true, nil
	}
	slug := a.Slug
	if marketdata.Slugify(slug) == "" {
		slug = a.Details.Name
	}
	if marketdata.Slugify(slug) == "" {
		slug = a.Details.Symbol
	}
	id, _, err := j.store.CreateAsset(ctx, slug, j.src.Name(), a.ExternalID, a.Details)
	if err != nil {
		return 0, false, err
	}
	if err := j.addContracts(ctx, run, id, a); err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// addContracts records the contracts of a, reporting each that the catalog
// gives to another asset.
func (j *Job) addContracts(ctx context.Context, run *jobs.Run, id int64, a source.Asset) error {
	for _, c := range a.Contracts {
		if run.DryRun() {
			continue
		}
		owner, err := j.store.AddContract(ctx, id, c.Network, c.Address, j.src.Name())
		if err != nil {
			return err
		}
		if owner != id {
			if err := j.conflict(ctx, run, marketdata.Conflict{Kind: marketdata.ConflictContract, ExternalID: a.ExternalID,
				Subject: c.Network + ":" + marketdata.NormalizeAddress(c.Address), AssetID: &owner, ClaimedBy: &id,
				Detail: fmt.Sprintf("%s gives this contract to %s, which the catalog has on another asset", j.src.Name(), a.ExternalID)}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (j *Job) conflict(ctx context.Context, run *jobs.Run, c marketdata.Conflict) error {
	if run.DryRun() {
		return nil
	}
	c.Source = j.src.Name()
	_, err := j.store.RecordConflict(ctx, c)
	return err
}

// readRecords reads the full records that are due, and writes their
// fields where this source owns the asset, and their contracts.
func (j *Job) readRecords(ctx context.Context, run *jobs.Run, now time.Time) error {
	if j.opts.DetailsPerRun <= 0 {
		return nil
	}
	ids, err := j.store.DetailsDue(ctx, j.src.Name(), now.Add(-j.opts.DetailsMaxAge), j.opts.DetailsPerRun)
	if err != nil || len(ids) == 0 {
		return err
	}
	records := map[string]source.Asset{}
	if reader, ok := j.src.(RecordReader); ok {
		if records, err = reader.Records(ctx, ids); err != nil {
			return fmt.Errorf("records: %w", err)
		}
	} else {
		for _, ext := range ids {
			a, err := j.src.Asset(ctx, ext)
			if errors.Is(err, source.ErrNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("record %s: %w", ext, err)
			}
			records[ext] = a
		}
	}
	run.AddRead(len(records))

	for i, ext := range ids {
		if i%batchSize == 0 {
			if err := run.Fence(ctx); err != nil {
				return err
			}
		}
		a, ok := records[ext]
		if !ok {
			// Gone from the source: read again after the maximum age.
			if !run.DryRun() {
				if err := j.store.MarkDetailsFetched(ctx, j.src.Name(), ext, now); err != nil {
					return err
				}
			}
			continue
		}
		if err := j.applyRecord(ctx, run, a, now); err != nil {
			return fmt.Errorf("record %s: %w", ext, err)
		}
	}
	return nil
}

func (j *Job) applyRecord(ctx context.Context, run *jobs.Run, a source.Asset, now time.Time) error {
	id, err := j.store.AssetBySource(ctx, j.src.Name(), a.ExternalID)
	if err != nil {
		return err
	}
	owns, err := j.owns(ctx, id)
	if err != nil {
		return err
	}
	if run.DryRun() {
		return nil
	}
	if owns {
		changed, err := j.store.UpdateRecord(ctx, id, marketdata.Record{
			Kind: a.Details.Kind, Description: a.Details.Description, Links: a.Details.Links, LogoURL: a.Details.LogoURL,
		})
		if err != nil {
			return err
		}
		if changed {
			run.AddWritten(1)
		}
	}
	if err := j.addContracts(ctx, run, id, a); err != nil {
		return err
	}
	return j.store.MarkDetailsFetched(ctx, j.src.Name(), a.ExternalID, now)
}
