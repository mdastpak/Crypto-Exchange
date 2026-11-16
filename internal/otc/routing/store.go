package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/rowseal"

	"github.com/shopspring/decimal"
)

// Version is a stored version of the rules, with who saved it and why.
type Version struct {
	RuleSet
	Reason    string    `json:"reason"`
	CreatedBy string    `json:"created_by" format:"uuid"`
	CreatedAt time.Time `json:"created_at"`
}

// Store keeps the versions of the rules in otc_routing_rules, and the
// daily usage of the venues in otc_routing_usage. Every node reads the
// same rows, so every node routes by the same version.
type Store struct {
	DB *sql.DB
	// Config gives the configuration that a version is validated against:
	// the venues and the region.
	Config func() *config.Configuration
}

func (s *Store) config() *config.Configuration {
	if s.Config == nil {
		return nil
	}
	return s.Config()
}

// Save validates rules and appends them as a new version, which applies
// on every node within its cache window, audited as
// admin.otc_routing_rules in the same transaction.
func (s *Store) Save(ctx context.Context, rules []Rule, reason string, origin audit.Event) (Version, error) {
	if rules == nil {
		rules = []Rule{}
	}
	if err := Validate(rules, s.config()); err != nil {
		return Version{}, err
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return Version{}, fmt.Errorf("routing: encode the rules: %w", err)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Version{}, fmt.Errorf("routing: save the rules: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	previous, err := latestVersion(ctx, tx)
	if err != nil {
		return Version{}, err
	}
	var version int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO otc_routing_rules (rules, reason, created_by) VALUES ($1::jsonb, $2, $3::uuid)
		RETURNING version`, raw, reason, origin.ActorID).Scan(&version); err != nil {
		return Version{}, fmt.Errorf("routing: save the rules: %w", err)
	}
	if err := rowseal.Reseal(ctx, tx, rowseal.OTCRoutingRules, "version = $1", version); err != nil {
		return Version{}, err
	}
	v, err := readVersion(ctx, tx, version, s.config())
	if err != nil {
		return Version{}, err
	}
	ev := audit.Of(origin, audit.ActionAdminOTCRoutingRules, audit.TargetOTCRoutingRules, strconv.FormatInt(version, 10))
	ev.Changes = &audit.Changes{
		Before: map[string]interface{}{"version": previous},
		After:  map[string]interface{}{"version": version, "rules": len(rules), "reason": reason},
	}
	if err := audit.Record(ctx, tx, ev); err != nil {
		return Version{}, err
	}
	if err := tx.Commit(); err != nil {
		return Version{}, fmt.Errorf("routing: save the rules: %w", err)
	}
	return v, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// latestVersion is the number of the highest version; 0 when there is
// none.
func latestVersion(ctx context.Context, q querier) (int64, error) {
	var v sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT max(version) FROM otc_routing_rules`).Scan(&v); err != nil {
		return 0, fmt.Errorf("routing: read the version of the rules: %w", err)
	}
	return v.Int64, nil
}

// Latest returns the highest version, checked against its seal: a version
// changed in SQL fails, and nothing routes until it is reviewed. No
// version is a RuleSet of version 0, which routes nothing.
func (s *Store) Latest(ctx context.Context) (Version, error) {
	v, err := latestVersion(ctx, s.DB)
	if err != nil || v == 0 {
		return Version{}, err
	}
	return readVersion(ctx, s.DB, v, s.config())
}

// Active implements RuleSource with the latest version, read each time,
// as the explanations of the administrators need right after a save.
func (s *Store) Active(ctx context.Context) (RuleSet, error) {
	v, err := s.Latest(ctx)
	return v.RuleSet, err
}

// ErrNotFound is a version that is not stored.
var ErrNotFound = errors.New("routing: no such version of the rules")

func readVersion(ctx context.Context, q querier, version int64, c *config.Configuration) (Version, error) {
	t := rowseal.OTCRoutingRules
	r := t.NewRow()
	var raw []byte
	var createdAt time.Time
	err := q.QueryRowContext(ctx, `SELECT rules, created_at, `+t.Fields("")+` FROM otc_routing_rules WHERE version = $1`, version).
		Scan(append([]any{&raw, &createdAt}, r.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	if err != nil {
		return Version{}, fmt.Errorf("routing: read the rules of version %d: %w", version, err)
	}
	if err := rowseal.Require(ctx, t, r); err != nil {
		return Version{}, err
	}
	set, err := Decode(version, raw, c)
	if err != nil {
		return Version{}, err
	}
	return Version{RuleSet: set, Reason: r.Values[t.Index("reason")].String, CreatedBy: r.Values[t.Index("created_by")].String,
		CreatedAt: createdAt.UTC()}, nil
}

// DailyNotional implements Usage.
func (s *Store) DailyNotional(ctx context.Context, venue, currencyHK string, day time.Time) (decimal.Decimal, error) {
	t := rowseal.OTCRoutingUsage
	r := t.NewRow()
	var notional decimal.Decimal
	err := s.DB.QueryRowContext(ctx, `SELECT notional::numeric, `+t.Fields("")+` FROM otc_routing_usage
		WHERE venue = $1 AND day = $2::date AND currency_hk = $3::uuid`, venue, day.UTC().Format(time.DateOnly), currencyHK).
		Scan(append([]any{&notional}, r.Dest()...)...)
	if errors.Is(err, sql.ErrNoRows) {
		return decimal.Zero, nil
	}
	if err != nil {
		return decimal.Zero, fmt.Errorf("routing: read the usage of %s: %w", venue, err)
	}
	if err := rowseal.Require(ctx, t, r); err != nil {
		return decimal.Zero, err
	}
	return notional, nil
}

// AddUsage adds notional to what venue took on day, in tx: the routed
// orders (#387) call it in the transaction that places them, so that the
// daily limits hold over every node.
func AddUsage(ctx context.Context, tx *sql.Tx, venue, currencyHK string, day time.Time, notional decimal.Decimal) error {
	if notional.IsNegative() {
		return fmt.Errorf("routing: a usage of %s is negative", notional)
	}
	d := day.UTC().Format(time.DateOnly)
	t := rowseal.OTCRoutingUsage
	var id string
	err := tx.QueryRowContext(ctx, `INSERT INTO otc_routing_usage (venue, day, currency_hk, notional)
		VALUES ($1, $2::date, $3::uuid, '0') ON CONFLICT (venue, day, currency_hk) DO NOTHING RETURNING id::text`,
		venue, d, currencyHK).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("routing: add to the usage of %s: %w", venue, err)
	default:
		if err := rowseal.Reseal(ctx, tx, t, "id = $1::uuid", id); err != nil {
			return err
		}
	}
	r := t.NewRow()
	var current decimal.Decimal
	if err := tx.QueryRowContext(ctx, `SELECT notional::numeric, `+t.Fields("")+` FROM otc_routing_usage
		WHERE venue = $1 AND day = $2::date AND currency_hk = $3::uuid FOR UPDATE`, venue, d, currencyHK).
		Scan(append([]any{&current}, r.Dest()...)...); err != nil {
		return fmt.Errorf("routing: add to the usage of %s: %w", venue, err)
	}
	if err := rowseal.Require(ctx, t, r); err != nil {
		return err
	}
	id = r.Key
	if _, err := tx.ExecContext(ctx, `UPDATE otc_routing_usage SET notional = $2::numeric WHERE id = $1::uuid`,
		id, current.Add(notional).String()); err != nil {
		return fmt.Errorf("routing: add to the usage of %s: %w", venue, err)
	}
	return rowseal.Reseal(ctx, tx, t, "id = $1::uuid", id)
}

// Cache keeps the active rules of one node. After its TTL it reads the
// number of the highest version, one small query, and reads the rules
// again only when it changed: a new version applies on every node within
// the TTL, without a restart. A read that fails, or a version that fails
// its seal, fails the routing, never routes by the rules held before.
type Cache struct {
	Store *Store
	TTL   time.Duration
	// Now is the clock of the TTL; nil is time.Now.
	Now func() time.Time

	mu      sync.Mutex
	set     RuleSet
	checked time.Time
	loaded  bool
}

// NewCache returns the cache of the store's rules, kept for ttl.
func NewCache(store *Store, ttl time.Duration) *Cache { return &Cache{Store: store, TTL: ttl} }

func (c *Cache) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Active implements RuleSource.
func (c *Cache) Active(ctx context.Context) (RuleSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.loaded && now.Sub(c.checked) < c.TTL {
		return c.set, nil
	}
	version, err := latestVersion(ctx, c.Store.DB)
	if err != nil {
		c.loaded = false
		return RuleSet{}, err
	}
	if !c.loaded || version != c.set.Version {
		set := RuleSet{}
		if version != 0 {
			v, err := readVersion(ctx, c.Store.DB, version, c.Store.config())
			if err != nil {
				c.loaded = false
				return RuleSet{}, err
			}
			set = v.RuleSet
		}
		c.set = set
	}
	c.checked, c.loaded = now, true
	return c.set, nil
}

// Invalidate makes the next Active read the version again, as after a
// save on this node.
func (c *Cache) Invalidate() {
	c.mu.Lock()
	c.loaded = false
	c.mu.Unlock()
}
