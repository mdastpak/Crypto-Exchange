package otc

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"
)

// InventoryCollector exports the positions of the exchange's inventory
// against otc.inventory_limits (#515), read from the database, so that
// every node reports the same. A trade checks the limits only on the
// inventory's own share. A venue leg whose remainder finds no route
// leaves its quantity on the inventory, past what the limits allowed;
// these gauges show it, and alert on it.
//
// A read serves every scrape for CacheFor (15 seconds when zero; negative
// reads at every scrape), and concurrent scrapes wait for one read.
type InventoryCollector struct {
	DB     *sql.DB
	Limits map[string]InventoryLimit
	// CacheFor is how long a read serves the scrapes.
	CacheFor time.Duration

	mu     sync.Mutex
	readAt time.Time
	got    []inventoryPosition
	ok     bool
}

// NewInventoryCollector returns the collector of the positions in db,
// against limits by currency symbol.
func NewInventoryCollector(db *sql.DB, limits map[string]InventoryLimit) *InventoryCollector {
	return &InventoryCollector{DB: db, Limits: limits}
}

type inventoryPosition struct {
	symbol   string
	position decimal.Decimal
}

var (
	inventoryPositionDesc = prometheus.NewDesc("otc_inventory_position",
		"The position of the exchange's inventory in a currency: above zero long, below zero short (it owes what users bought).",
		[]string{"currency"}, nil)
	inventoryLimitDesc = prometheus.NewDesc("otc_inventory_limit",
		"The bound of otc.inventory_limits of a currency, by bound (max_long, max_short), as a positive amount.",
		[]string{"currency", "bound"}, nil)
	inventoryBeyondDesc = prometheus.NewDesc("otc_inventory_beyond_limit",
		"1 when the position of a currency is past a bound of otc.inventory_limits, such as a venue leg left to the inventory; 0 otherwise.",
		[]string{"currency"}, nil)
)

// Describe implements prometheus.Collector.
func (c *InventoryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- inventoryPositionDesc
	ch <- inventoryLimitDesc
	ch <- inventoryBeyondDesc
}

// Collect implements prometheus.Collector. A failed read exports nothing.
func (c *InventoryCollector) Collect(ch chan<- prometheus.Metric) {
	got, ok := c.read()
	if !ok {
		return
	}
	for _, p := range got {
		ch <- prometheus.MustNewConstMetric(inventoryPositionDesc, prometheus.GaugeValue, p.position.InexactFloat64(), p.symbol)
		l, limited := c.Limits[p.symbol]
		if !limited {
			continue
		}
		if l.MaxLong != nil {
			ch <- prometheus.MustNewConstMetric(inventoryLimitDesc, prometheus.GaugeValue, l.MaxLong.InexactFloat64(), p.symbol, "max_long")
		}
		if l.MaxShort != nil {
			ch <- prometheus.MustNewConstMetric(inventoryLimitDesc, prometheus.GaugeValue, l.MaxShort.InexactFloat64(), p.symbol, "max_short")
		}
		beyond := 0.0
		if Beyond(p.position, l) {
			beyond = 1
		}
		ch <- prometheus.MustNewConstMetric(inventoryBeyondDesc, prometheus.GaugeValue, beyond, p.symbol)
	}
}

// Beyond reports whether position is past a bound of l: longer than
// MaxLong, or shorter than MaxShort.
func Beyond(position decimal.Decimal, l InventoryLimit) bool {
	return l.MaxLong != nil && position.GreaterThan(*l.MaxLong) || l.MaxShort != nil && position.Neg().GreaterThan(*l.MaxShort)
}

func (c *InventoryCollector) read() ([]inventoryPosition, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cacheFor := c.CacheFor
	if cacheFor == 0 {
		cacheFor = 15 * time.Second
	}
	if c.ok && cacheFor > 0 && time.Since(c.readAt) < cacheFor {
		return c.got, true
	}
	c.ok = false
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := c.DB.QueryContext(ctx, `SELECT COALESCE(c.info->>'symbol', ''), p.position::text FROM otc_house_positions p
		JOIN currencies c ON c.hk = p.currency_hk ORDER BY 1`)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var got []inventoryPosition
	for rows.Next() {
		var symbol, position string
		if err := rows.Scan(&symbol, &position); err != nil {
			return nil, false
		}
		d, err := decimal.NewFromString(position)
		if err != nil {
			return nil, false
		}
		got = append(got, inventoryPosition{symbol: strings.ToUpper(symbol), position: d})
	}
	if rows.Err() != nil {
		return nil, false
	}
	c.got, c.readAt, c.ok = got, time.Now(), true
	return got, true
}
