package routed

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"
)

// Collector exports the routed orders as Prometheus gauges (#513), read
// from the database, so that every node reports the same: the orders that
// are open or wait for a person, by venue and status, how old the oldest
// is, the notional they leave unhedged, the time from plan to fill over
// the last hour, and the orders that fail their seal.
//
// A read is three small queries over partial indexes. It serves every
// scrape for CacheFor, and concurrent scrapes wait for one read. Use
// NewCollector.
type Collector struct {
	DB *sql.DB
	// Timeout bounds the reads of one scrape; 5 seconds when zero.
	Timeout time.Duration
	// CacheFor is how long a read serves the scrapes; 15 seconds when
	// zero. A negative value reads at every scrape.
	CacheFor time.Duration

	mu     sync.Mutex
	readAt time.Time
	got    collected
	ok     bool
	// clock is the local clock of the cache; time.Now when nil.
	clock func() time.Time
}

// NewCollector returns a Collector of the routed orders in db.
func NewCollector(db *sql.DB) *Collector { return &Collector{DB: db} }

// DefaultCacheFor is how long a read of the Collector serves the scrapes.
const DefaultCacheFor = 15 * time.Second

// FillWindow is the window of the fill times.
const FillWindow = time.Hour

var (
	ordersDesc = prometheus.NewDesc("otc_routed_orders",
		"Routed orders that a node still drives (planned, placed, partially_filled) or that wait for a person (manual_review), by venue and status.",
		[]string{"venue", "status"}, nil)
	oldestDesc = prometheus.NewDesc("otc_routed_order_oldest_seconds",
		"Seconds since the oldest of these orders was planned, by venue and status.",
		[]string{"venue", "status"}, nil)
	unhedgedDesc = prometheus.NewDesc("otc_routed_order_unhedged",
		"What these orders have not filled, at their reference price, in the venue's quote asset: the notional the inventory still carries, by venue, status, and quote.",
		[]string{"venue", "status", "quote"}, nil)
	fillDesc = prometheus.NewDesc("otc_routed_order_fill_seconds",
		"Seconds from the plan of an order to its fill, over the orders filled in the last hour, by venue and quantile (0.5, 0.95, 1 for the slowest).",
		[]string{"venue", "quantile"}, nil)
	unverifiedDesc = prometheus.NewDesc("otc_routed_orders_unverified",
		"Routed orders with an open integrity finding: changed outside the backend, not driven nor resolvable until re-sealed after a review.",
		nil, nil)
)

type groupKey struct{ venue, status string }

type group struct {
	count  int
	oldest float64
}

type fillTimes struct{ p50, p95, max float64 }

type collected struct {
	groups     map[groupKey]group
	unhedged   map[[3]string]decimal.Decimal
	fills      map[string]fillTimes
	unverified int
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- ordersDesc
	ch <- oldestDesc
	ch <- unhedgedDesc
	ch <- fillDesc
	ch <- unverifiedDesc
}

// Collect implements prometheus.Collector. A failed read exports nothing
// for the scrape, and is not cached. A venue and status with no order
// export no series: alert on the series being above zero, not on its
// value being zero.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	got, ok := c.read()
	if !ok {
		return
	}
	for k, g := range got.groups {
		ch <- prometheus.MustNewConstMetric(ordersDesc, prometheus.GaugeValue, float64(g.count), k.venue, k.status)
		ch <- prometheus.MustNewConstMetric(oldestDesc, prometheus.GaugeValue, g.oldest, k.venue, k.status)
	}
	for k, v := range got.unhedged {
		ch <- prometheus.MustNewConstMetric(unhedgedDesc, prometheus.GaugeValue, v.InexactFloat64(), k[0], k[1], k[2])
	}
	for venue, f := range got.fills {
		ch <- prometheus.MustNewConstMetric(fillDesc, prometheus.GaugeValue, f.p50, venue, "0.5")
		ch <- prometheus.MustNewConstMetric(fillDesc, prometheus.GaugeValue, f.p95, venue, "0.95")
		ch <- prometheus.MustNewConstMetric(fillDesc, prometheus.GaugeValue, f.max, venue, "1")
	}
	ch <- prometheus.MustNewConstMetric(unverifiedDesc, prometheus.GaugeValue, float64(got.unverified))
}

func (c *Collector) read() (collected, bool) {
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
		return c.got, true
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c.ok = false
	got, err := collect(ctx, c.DB)
	if err != nil {
		return collected{}, false
	}
	c.got, c.readAt, c.ok = got, clock(), true
	return got, true
}

// collect reads what the Collector exports.
func collect(ctx context.Context, db *sql.DB) (collected, error) {
	got := collected{groups: map[groupKey]group{}, unhedged: map[[3]string]decimal.Decimal{}, fills: map[string]fillTimes{}}
	rows, err := db.QueryContext(ctx, `SELECT venue, status, quote_asset, count(*),
			EXTRACT(EPOCH FROM clock_timestamp() - min(created_at))::float8,
			COALESCE(sum((quantity::numeric - filled::numeric) * reference_price::numeric), 0)::text
		FROM otc_routed_orders WHERE status IN ('planned', 'placed', 'partially_filled', 'manual_review')
		GROUP BY venue, status, quote_asset`)
	if err != nil {
		return got, err
	}
	for rows.Next() {
		var k groupKey
		var quote, notional string
		var n int
		var oldest float64
		if err := rows.Scan(&k.venue, &k.status, &quote, &n, &oldest, &notional); err != nil {
			rows.Close()
			return got, err
		}
		g := got.groups[k]
		g.count += n
		g.oldest = max(g.oldest, oldest)
		got.groups[k] = g
		v, err := decimal.NewFromString(notional)
		if err != nil {
			rows.Close()
			return got, err
		}
		key := [3]string{k.venue, k.status, quote}
		got.unhedged[key] = got.unhedged[key].Add(v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return got, err
	}
	rows, err = db.QueryContext(ctx, `SELECT venue,
			percentile_cont(0.5) WITHIN GROUP (ORDER BY s), percentile_cont(0.95) WITHIN GROUP (ORDER BY s), max(s)
		FROM (SELECT o.venue, EXTRACT(EPOCH FROM e.at - o.created_at)::float8 AS s
			FROM otc_routed_order_events e JOIN otc_routed_orders o ON o.id = e.order_id
			WHERE e.to_status = 'filled' AND e.at > clock_timestamp() - make_interval(secs => $1)) f
		GROUP BY venue`, FillWindow.Seconds())
	if err != nil {
		return got, err
	}
	for rows.Next() {
		var venue string
		var f fillTimes
		if err := rows.Scan(&venue, &f.p50, &f.p95, &f.max); err != nil {
			rows.Close()
			return got, err
		}
		got.fills[venue] = f
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return got, err
	}
	err = db.QueryRowContext(ctx, `SELECT count(DISTINCT row_key) FROM integrity_findings
		WHERE table_name = 'otc_routed_orders' AND resolved_at IS NULL`).Scan(&got.unverified)
	return got, err
}
