package state

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"
	"github.com/mdastpak/crypto-exchange/internal/venues"

	"github.com/prometheus/client_golang/prometheus"
)

// Snapshot is the last snapshot of one kind of a venue.
type Snapshot struct {
	Kind     string     `json:"kind"`
	TakenAt  *time.Time `json:"taken_at,omitempty"`
	FailedAt *time.Time `json:"failed_at,omitempty"`
	// Failure is the kind of the last failure: retryable, rejected, or
	// unknown_outcome.
	Failure string `json:"failure,omitempty"`
	Fresh   bool   `json:"fresh"`
}

// BalanceState is one currency of a venue against the books.
type BalanceState struct {
	Asset    string `json:"asset"`
	Currency string `json:"currency,omitempty"`
	// Free and Locked are the last snapshot; Reserved the open
	// reservations; Available what the router may still plan on.
	Free      shared.Decimal `json:"free"`
	Locked    shared.Decimal `json:"locked"`
	Reserved  shared.Decimal `json:"reserved"`
	Available shared.Decimal `json:"available"`
	// Booked is what the books say the venue holds; Difference is
	// Free + Locked - Booked.
	Booked       shared.Decimal `json:"booked"`
	Difference   shared.Decimal `json:"difference"`
	MismatchRuns int            `json:"mismatch_runs"`
	TakenAt      time.Time      `json:"taken_at"`
	Routability  Routability    `json:"routability"`
}

// NetworkState is a network of an asset at a venue.
type NetworkState struct {
	Asset           string         `json:"asset"`
	Network         string         `json:"network"`
	DepositEnabled  bool           `json:"deposit_enabled"`
	WithdrawEnabled bool           `json:"withdraw_enabled"`
	WithdrawFee     shared.Decimal `json:"withdraw_fee"`
	WithdrawMin     shared.Decimal `json:"withdraw_min"`
	TakenAt         time.Time      `json:"taken_at"`
}

// VenueState is what the admin sees of a venue (#71).
type VenueState struct {
	Venue     string         `json:"venue"`
	Snapshots []Snapshot     `json:"snapshots"`
	Balances  []BalanceState `json:"balances"`
	Networks  []NetworkState `json:"networks"`
	// Instruments and Trading count the markets listed, and those trading.
	Instruments int    `json:"instruments"`
	Trading     int    `json:"trading"`
	Holds       []Hold `json:"holds"`
}

// Overview returns the state of the venues of mappings, by name; mappings
// gives our currency of each asset.
func (s *Store) Overview(ctx context.Context, mappings map[string]venues.Mapping) ([]VenueState, error) {
	names := make([]string, 0, len(mappings))
	for name := range mappings {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]VenueState, 0, len(names))
	for _, name := range names {
		v, err := s.venue(ctx, name, mappings[name])
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) venue(ctx context.Context, name string, mapping venues.Mapping) (VenueState, error) {
	v := VenueState{Venue: name, Snapshots: []Snapshot{}, Balances: []BalanceState{}, Networks: []NetworkState{}, Holds: []Hold{}}
	fail := func(err error) (VenueState, error) {
		return VenueState{}, fmt.Errorf("venues: read the state of %s: %w", name, err)
	}

	byKind := map[string]Snapshot{}
	now := s.Clock(ctx)
	rows, err := s.DB.QueryContext(ctx, `SELECT kind, taken_at, failed_at, COALESCE(failure, '') FROM venue_snapshots WHERE venue = $1`, name)
	if err != nil {
		return fail(err)
	}
	for rows.Next() {
		var sn Snapshot
		if err := rows.Scan(&sn.Kind, &sn.TakenAt, &sn.FailedAt, &sn.Failure); err != nil {
			rows.Close()
			return fail(err)
		}
		sn.Fresh = sn.TakenAt != nil && s.fresh(now, *sn.TakenAt)
		byKind[sn.Kind] = sn
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail(err)
	}
	for _, kind := range []string{KindBalances, KindInstruments, KindNetworks, KindReconciliation} {
		sn, ok := byKind[kind]
		if !ok {
			sn = Snapshot{Kind: kind}
		}
		v.Snapshots = append(v.Snapshots, sn)
	}

	booked, err := s.Ledger.VenueHoldings(ctx, name)
	if err != nil {
		return VenueState{}, err
	}
	rows, err = s.DB.QueryContext(ctx, `SELECT asset, free, locked, mismatch_runs, taken_at FROM venue_balances WHERE venue = $1 ORDER BY asset`, name)
	if err != nil {
		return fail(err)
	}
	for rows.Next() {
		var b BalanceState
		if err := rows.Scan(&b.Asset, &b.Free, &b.Locked, &b.MismatchRuns, &b.TakenAt); err != nil {
			rows.Close()
			return fail(err)
		}
		v.Balances = append(v.Balances, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail(err)
	}
	for i := range v.Balances {
		b := &v.Balances[i]
		if symbol, _, ok := mapping.Ours(b.Asset, ""); ok {
			b.Currency = symbol
			if hk, err := currencyHK(ctx, s.DB, symbol); err == nil {
				b.Booked = booked[hk.String()]
			}
		}
		_, reserved, err := openReservations(ctx, s.DB, name, b.Asset)
		unverified := errors.Is(err, rowseal.ErrUnverified)
		if err != nil && !unverified {
			return VenueState{}, err
		}
		b.Reserved, b.Available = reserved, b.Free.Sub(reserved)
		b.Difference = b.Free.Add(b.Locked).Sub(b.Booked)
		if b.Routability, err = s.Routable(ctx, name, b.Asset, ""); err != nil {
			return VenueState{}, err
		}
		if unverified {
			// A reservation changed outside the backend (#504): what is
			// reserved is unknown, so nothing is available, and the
			// router refuses the balance as ReserveIn does. The other
			// balances and venues still show.
			b.Reserved, b.Available = shared.Decimal{}, shared.Decimal{}
			b.Routability.not(ReasonUnverifiedReservation)
		}
		if b.Currency == "" {
			b.Routability.not("not_mapped")
		}
	}

	rows, err = s.DB.QueryContext(ctx, `SELECT asset, network, deposit_enabled, withdraw_enabled, withdraw_fee, withdraw_min, taken_at
		FROM venue_networks WHERE venue = $1 ORDER BY asset, network`, name)
	if err != nil {
		return fail(err)
	}
	for rows.Next() {
		var n NetworkState
		if err := rows.Scan(&n.Asset, &n.Network, &n.DepositEnabled, &n.WithdrawEnabled, &n.WithdrawFee, &n.WithdrawMin, &n.TakenAt); err != nil {
			rows.Close()
			return fail(err)
		}
		v.Networks = append(v.Networks, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*), count(*) FILTER (WHERE trading) FROM venue_instruments WHERE venue = $1`,
		name).Scan(&v.Instruments, &v.Trading); err != nil {
		return fail(err)
	}

	rows, err = s.DB.QueryContext(ctx, `SELECT id::text FROM venue_holds WHERE venue = $1 AND cleared_at IS NULL ORDER BY detected_at`, name)
	if err != nil {
		return fail(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fail(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fail(err)
	}
	for _, id := range ids {
		h, err := readHold(ctx, s.DB, id)
		if err != nil {
			return VenueState{}, err
		}
		v.Holds = append(v.Holds, h)
	}
	return v, nil
}

// Collector exports the state of the venues as Prometheus gauges, read
// from the database, so that every node reports the same: the age of each
// snapshot, the reconciliation difference, and the routable balance of
// each currency.
//
// One read of the state costs a few queries per venue and per balance
// (#504). A read serves every scrape for CacheFor, and concurrent scrapes
// wait for one read, so each instance reads at most once per CacheFor,
// however often or from however many scrapers it is scraped. Use
// NewCollector: the cache lives in the Collector.
type Collector struct {
	Store    *Store
	Mappings map[string]venues.Mapping
	// Timeout bounds the reads of one scrape; 5 seconds when zero.
	Timeout time.Duration
	// CacheFor is how long a read serves the scrapes; 15 seconds when
	// zero. A negative value reads at every scrape.
	CacheFor time.Duration

	mu     sync.Mutex
	readAt time.Time
	states []VenueState
	now    time.Time
	ok     bool
	// clock is the local clock of the cache; time.Now when nil.
	clock func() time.Time
}

// NewCollector returns a Collector of the venues of mappings.
func NewCollector(store *Store, mappings map[string]venues.Mapping) *Collector {
	return &Collector{Store: store, Mappings: mappings}
}

// DefaultCacheFor is how long a read of the Collector serves the scrapes.
const DefaultCacheFor = 15 * time.Second

var (
	snapshotAge = prometheus.NewDesc("venue_snapshot_age_seconds",
		"Seconds since the last snapshot of a venue, by venue and kind (balances, instruments, networks, reconciliation).",
		[]string{"venue", "kind"}, nil)
	reconcileDifference = prometheus.NewDesc("venue_reconciliation_difference",
		"What a venue reports holding less what the books say it holds, by venue and asset.",
		[]string{"venue", "asset"}, nil)
	routableBalance = prometheus.NewDesc("venue_routable_balance",
		"The free balance less the open reservations of a currency at a venue, 0 while it is not routable, by venue and asset.",
		[]string{"venue", "asset"}, nil)
	openHolds = prometheus.NewDesc("venue_open_holds",
		"The open reconciliation holds of a venue.", []string{"venue"}, nil)
)

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- snapshotAge
	ch <- reconcileDifference
	ch <- routableBalance
	ch <- openHolds
}

// Collect implements prometheus.Collector. A failed read exports nothing
// for the scrape: an absent series alerts as stale. A failed read is not
// cached, so the next scrape reads again.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	states, now, ok := c.read()
	if !ok {
		return
	}
	for _, v := range states {
		for _, sn := range v.Snapshots {
			if sn.TakenAt != nil {
				ch <- prometheus.MustNewConstMetric(snapshotAge, prometheus.GaugeValue, now.Sub(*sn.TakenAt).Seconds(), v.Venue, sn.Kind)
			}
		}
		for _, b := range v.Balances {
			ch <- prometheus.MustNewConstMetric(reconcileDifference, prometheus.GaugeValue, b.Difference.InexactFloat64(), v.Venue, b.Asset)
			routable := 0.0
			if b.Routability.Routable {
				routable = b.Available.InexactFloat64()
			}
			ch <- prometheus.MustNewConstMetric(routableBalance, prometheus.GaugeValue, routable, v.Venue, b.Asset)
		}
		ch <- prometheus.MustNewConstMetric(openHolds, prometheus.GaugeValue, float64(len(v.Holds)), v.Venue)
	}
}

// read returns the state of the venues and the database time of its read,
// from the cache while it is younger than CacheFor. The snapshot ages are
// of the time of the read: they lag by at most CacheFor.
func (c *Collector) read() ([]VenueState, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clock := c.clock
	if clock == nil {
		clock = time.Now
	}
	cacheFor := c.CacheFor
	if cacheFor == 0 {
		cacheFor = DefaultCacheFor
	}
	if c.ok && cacheFor > 0 && clock().Sub(c.readAt) < cacheFor {
		return c.states, c.now, true
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c.ok = false
	states, err := c.Store.Overview(ctx, c.Mappings)
	if err != nil {
		return nil, time.Time{}, false
	}
	c.states, c.now, c.readAt, c.ok = states, c.Store.Clock(ctx), clock(), true
	return c.states, c.now, true
}
