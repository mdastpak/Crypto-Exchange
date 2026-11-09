// Package geoip finds the country of a client (#284), so that a visitor can
// be offered the site and language of their country (#285).
//
// Two sources answer, in this order:
//
//   - the country header of a CDN or load balancer in front of the server,
//     such as Cloudflare's CF-IPCountry, trusted only from the trusted
//     proxies, so that a client cannot name its own country;
//   - a MaxMind-format country database (.mmdb), such as GeoLite2-Country
//     or DB-IP Lite Country, which is reloaded when its file changes.
//
// The country is a suggestion, never a security decision: addresses move
// between countries, VPNs hide them, and the databases are a few days old.
package geoip

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oschwald/maxminddb-golang/v2"
)

// Where the country of a request came from.
const (
	SourceHeader   = "header"
	SourceDatabase = "database"
	SourceOverride = "override"
	SourceNone     = "none"
)

// Result is the country of a request.
type Result struct {
	// Country is the ISO 3166-1 alpha-2 code, upper case, or "" when it is
	// not known.
	Country string `json:"country" example:"IR"`
	// Source is where it came from, one of the Source constants.
	Source string `json:"source" example:"database" enums:"header,database,override,none"`
}

// Options configure a Resolver.
type Options struct {
	// Header names the request header that carries the country, set by a
	// proxy in front of the server. Empty means no header source.
	Header string
	// TrustedProxies are the addresses and networks whose Header is
	// trusted: the server.trusted_proxies of the configuration.
	TrustedProxies []string
	// DatabasePath is the .mmdb file. Empty means no database source.
	DatabasePath string
	// Overrides map networks to countries, for trying the flow locally.
	// They come before the other sources.
	Overrides map[string]string
	// MaxAge is the age of the database after which Check reports it as
	// stale. Zero means DefaultMaxAge.
	MaxAge time.Duration
}

// DefaultMaxAge is the age past which a database counts as stale: the free
// databases are updated monthly or more often.
const DefaultMaxAge = 45 * 24 * time.Hour

// Resolver finds the country of requests. It is safe for concurrent use.
// A nil *Resolver knows no country.
type Resolver struct {
	header    string
	trusted   []netip.Prefix
	path      string
	overrides []override
	maxAge    time.Duration

	db atomic.Pointer[database]
	// mu serialises reloads.
	mu sync.Mutex
	// loadErr is the error of the last load, for Check.
	loadErr atomic.Pointer[error]
}

type override struct {
	network netip.Prefix
	country string
}

// database is one loaded file. It is read into memory, so that a new file
// can replace it while lookups are running, without unmapping memory that
// they still read.
type database struct {
	reader  *maxminddb.Reader
	built   time.Time
	kind    string
	modTime time.Time
	size    int64
}

// New builds a resolver. It does not load the database; Load does.
func New(opts Options) (*Resolver, error) {
	r := &Resolver{
		header: strings.TrimSpace(opts.Header),
		path:   strings.TrimSpace(opts.DatabasePath),
		maxAge: opts.MaxAge,
	}
	if r.maxAge <= 0 {
		r.maxAge = DefaultMaxAge
	}
	for _, entry := range opts.TrustedProxies {
		prefix, err := ParsePrefix(entry)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %w", err)
		}
		r.trusted = append(r.trusted, prefix)
	}
	for network, country := range opts.Overrides {
		prefix, err := ParsePrefix(network)
		if err != nil {
			return nil, fmt.Errorf("override %w", err)
		}
		code, ok := NormalizeCountry(country)
		if !ok {
			return nil, fmt.Errorf("override of %s: %q is not an ISO 3166-1 alpha-2 country code", network, country)
		}
		r.overrides = append(r.overrides, override{prefix, code})
	}
	return r, nil
}

// ParsePrefix reads an address or a network in CIDR notation, as
// server.trusted_proxies lists them.
func ParsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%q is not an address or a network", s)
		}
		return prefix.Masked(), nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%q is not an address or a network", s)
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// notCountries are codes that some sources send for no country: unknown,
// and the continents of older databases. Tor (T1) and the anonymous
// proxies of older databases (A1, A2, O1) are not two letters.
var notCountries = map[string]bool{"XX": true, "ZZ": true, "EU": true, "AP": true}

// NormalizeCountry returns code in upper case when it is a two-letter
// country code, and false for anything else.
func NormalizeCountry(code string) (string, bool) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' || notCountries[code] {
		return "", false
	}
	return code, true
}

// Load reads the database, if one is configured, and replaces the current
// one. A file that cannot be read or is not a country database keeps the
// current one; Load returns why.
func (r *Resolver) Load() error {
	if r == nil || r.path == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	err := r.load()
	if err != nil {
		r.loadErr.Store(&err)
	} else {
		r.loadErr.Store(nil)
	}
	return err
}

func (r *Resolver) load() error {
	info, err := os.Stat(r.path)
	if err != nil {
		return fmt.Errorf("geoip database: %w", err)
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		return fmt.Errorf("geoip database: %w", err)
	}
	reader, err := maxminddb.OpenBytes(data)
	if err != nil {
		return fmt.Errorf("geoip database %s: %w", r.path, err)
	}
	kind := reader.Metadata.DatabaseType
	if !strings.Contains(strings.ToLower(kind), "country") && !strings.Contains(strings.ToLower(kind), "city") {
		return fmt.Errorf("geoip database %s is a %q database; it must be a country or city database", r.path, kind)
	}
	r.db.Store(&database{
		reader:  reader,
		built:   reader.Metadata.BuildTime().UTC(),
		kind:    kind,
		modTime: info.ModTime(),
		size:    info.Size(),
	})
	return nil
}

// changed reports whether the file differs from the loaded one.
func (r *Resolver) changed() bool {
	info, err := os.Stat(r.path)
	if err != nil {
		return false
	}
	current := r.db.Load()
	return current == nil || !info.ModTime().Equal(current.modTime) || info.Size() != current.size
}

// Watch reloads the database when its file changes, checking every
// interval, until ctx ends. A tool such as geoipupdate can therefore
// replace the file without a restart. Each instance watches its own file.
func (r *Resolver) Watch(ctx context.Context, interval time.Duration) {
	if r == nil || r.path == "" || interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !r.changed() {
			continue
		}
		if err := r.Load(); err != nil {
			log.Printf("GeoIP: keeping the loaded database: %v", err)
			continue
		}
		if s := r.Status(); s.Database != nil {
			log.Printf("GeoIP: loaded %s, built %s", s.Database.Type, s.Database.BuiltAt.Format(time.DateOnly))
		}
	}
}

// Lookup returns the country of a request whose client address Gin
// resolved as clientIP, through the trusted proxies, and whose TCP peer is
// remoteIP.
func (r *Resolver) Lookup(req *http.Request, clientIP, remoteIP string) Result {
	none := Result{Source: SourceNone}
	if r == nil {
		return none
	}
	client, clientErr := netip.ParseAddr(clientIP)
	client = client.Unmap()
	if clientErr == nil {
		for _, o := range r.overrides {
			if o.network.Contains(client) {
				return Result{Country: o.country, Source: SourceOverride}
			}
		}
	}
	if r.header != "" && req != nil && r.trustedPeer(remoteIP) {
		if country, ok := NormalizeCountry(req.Header.Get(r.header)); ok {
			return Result{Country: country, Source: SourceHeader}
		}
	}
	if clientErr != nil || !Public(client) {
		return none
	}
	if country := r.lookupDatabase(client); country != "" {
		return Result{Country: country, Source: SourceDatabase}
	}
	return none
}

func (r *Resolver) trustedPeer(remoteIP string) bool {
	peer, err := netip.ParseAddr(remoteIP)
	if err != nil {
		return false
	}
	peer = peer.Unmap()
	for _, prefix := range r.trusted {
		if prefix.Contains(peer) {
			return true
		}
	}
	return false
}

// countryRecord is the part of a country or city record that names the
// country. A network without a country, such as an anycast or satellite
// network, has only the registered country.
type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
}

func (r *Resolver) lookupDatabase(addr netip.Addr) string {
	db := r.db.Load()
	if db == nil {
		return ""
	}
	var record countryRecord
	if err := db.reader.Lookup(addr).Decode(&record); err != nil {
		return ""
	}
	if country, ok := NormalizeCountry(record.Country.ISOCode); ok {
		return country
	}
	country, _ := NormalizeCountry(record.RegisteredCountry.ISOCode)
	return country
}

// cgnat is the shared address space of carrier-grade NAT (RFC 6598).
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Public reports whether addr can have a country: not private, loopback,
// link-local, shared (CGNAT), multicast, or unspecified.
func Public(addr netip.Addr) bool {
	return addr.IsValid() && addr.IsGlobalUnicast() && !addr.IsPrivate() && !cgnat.Contains(addr)
}

// ginKey keeps the result of a request, so that it is looked up once.
const ginKey = "geoip.result"

// Resolve returns the country of a Gin request, looked up once per
// request.
func (r *Resolver) Resolve(c *gin.Context) Result {
	if v, ok := c.Get(ginKey); ok {
		if result, ok := v.(Result); ok {
			return result
		}
	}
	result := r.Lookup(c.Request, c.ClientIP(), c.RemoteIP())
	c.Set(ginKey, result)
	return result
}

// Status describes the sources of a resolver, for the health checks.
type Status struct {
	Header   string          `json:"header,omitempty"`
	Database *DatabaseStatus `json:"database,omitempty"`
}

// DatabaseStatus describes the loaded database.
type DatabaseStatus struct {
	Type    string    `json:"type"`
	BuiltAt time.Time `json:"built_at"`
}

// Status returns the sources that the resolver has now.
func (r *Resolver) Status() Status {
	if r == nil {
		return Status{}
	}
	s := Status{Header: r.header}
	if db := r.db.Load(); db != nil {
		s.Database = &DatabaseStatus{Type: db.kind, BuiltAt: db.built}
	}
	return s
}

// ErrStale reports a database older than the maximum age.
var ErrStale = errors.New("the geoip database is stale")

// Check reports a database that is configured but not loaded, whose last
// reload failed, or that is older than the maximum age, for the readiness
// check. It never fails the instance: a stale database only makes
// suggestions worse.
func (r *Resolver) Check(now time.Time) error {
	if r == nil || r.path == "" {
		return nil
	}
	if err := r.loadErr.Load(); err != nil && *err != nil {
		return *err
	}
	db := r.db.Load()
	if db == nil {
		return errors.New("the geoip database is not loaded")
	}
	if now.Sub(db.built) > r.maxAge {
		return fmt.Errorf("%w: built %s", ErrStale, db.built.Format(time.DateOnly))
	}
	return nil
}
