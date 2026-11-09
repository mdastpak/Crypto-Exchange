package geoip

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeDatabase writes a country database of networks to path, built at
// built, as GeoLite2-Country and DB-IP Lite are laid out. An entry with
// country "" has only a registered country, "registered:XX".
func writeDatabase(t *testing.T, path, kind string, built time.Time, networks map[string]string) {
	t.Helper()
	tree, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: kind, BuildEpoch: built.Unix(), RecordSize: 24})
	require.NoError(t, err)
	for network, country := range networks {
		_, ipnet, err := net.ParseCIDR(network)
		require.NoError(t, err)
		record := mmdbtype.Map{}
		if registered, ok := cutPrefix(country, "registered:"); ok {
			record["registered_country"] = mmdbtype.Map{"iso_code": mmdbtype.String(registered)}
		} else {
			record["country"] = mmdbtype.Map{"iso_code": mmdbtype.String(country)}
		}
		require.NoError(t, tree.Insert(ipnet, record))
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	require.NoError(t, err)
	_, err = tree.WriteTo(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	// Like geoipupdate, replace the file in one rename.
	require.NoError(t, os.Rename(tmp, path))
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return s, false
}

var testNetworks = map[string]string{
	"81.12.0.0/16":     "IR",
	"8.8.8.0/24":       "us",
	"2a01:5ec0::/32":   "IR",
	"2001:4860::/32":   "US",
	"185.199.108.0/22": "registered:NL",
}

func newDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "country.mmdb")
	writeDatabase(t, path, "GeoLite2-Country", time.Now(), testNetworks)
	return path
}

func TestLookup_Database(t *testing.T) {
	r, err := New(Options{DatabasePath: newDatabase(t)})
	require.NoError(t, err)
	require.NoError(t, r.Load())

	for ip, want := range map[string]Result{
		"81.12.34.56":          {"IR", SourceDatabase},
		"::ffff:81.12.34.56":   {"IR", SourceDatabase},
		"8.8.8.8":              {"US", SourceDatabase},
		"2a01:5ec0:1::1":       {"IR", SourceDatabase},
		"2001:4860:4860::8888": {"US", SourceDatabase},
		"185.199.109.153":      {"NL", SourceDatabase},
		"1.1.1.1":              {"", SourceNone},
		// Addresses that have no country, even when a database lists
		// them.
		"10.1.2.3":    {"", SourceNone},
		"192.168.1.1": {"", SourceNone},
		"127.0.0.1":   {"", SourceNone},
		"100.64.1.1":  {"", SourceNone},
		"169.254.1.1": {"", SourceNone},
		"fd00::1":     {"", SourceNone},
		"fe80::1":     {"", SourceNone},
		"::1":         {"", SourceNone},
		"not an ip":   {"", SourceNone},
	} {
		assert.Equal(t, want, r.Lookup(nil, ip, ip), ip)
	}
	status := r.Status()
	require.NotNil(t, status.Database)
	assert.Equal(t, "GeoLite2-Country", status.Database.Type)
	assert.NoError(t, r.Check(time.Now()))
}

func TestLookup_HeaderOnlyFromTrustedProxies(t *testing.T) {
	r, err := New(Options{
		Header:         "CF-IPCountry",
		TrustedProxies: []string{"10.0.0.0/8", "2001:db8::1"},
		DatabasePath:   newDatabase(t),
	})
	require.NoError(t, err)
	require.NoError(t, r.Load())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("CF-IPCountry", "de")

	// From the proxy, the header wins over the database.
	assert.Equal(t, Result{"DE", SourceHeader}, r.Lookup(req, "81.12.1.1", "10.1.1.1"))
	assert.Equal(t, Result{"DE", SourceHeader}, r.Lookup(req, "81.12.1.1", "2001:db8::1"))
	assert.Equal(t, Result{"DE", SourceHeader}, r.Lookup(req, "81.12.1.1", "::ffff:10.1.1.1"))
	// A client that sends the header itself is not believed.
	assert.Equal(t, Result{"IR", SourceDatabase}, r.Lookup(req, "81.12.1.1", "81.12.1.1"))
	assert.Equal(t, Result{"IR", SourceDatabase}, r.Lookup(req, "81.12.1.1", "2001:db8::2"))

	// Unknown and Tor answer no country, so the database decides.
	for _, value := range []string{"XX", "T1", "", "EU", "IRN", "1R"} {
		req.Header.Set("CF-IPCountry", value)
		assert.Equal(t, Result{"IR", SourceDatabase}, r.Lookup(req, "81.12.1.1", "10.1.1.1"), value)
	}
	// Without a database and a header, nothing.
	r, err = New(Options{Header: "CF-IPCountry", TrustedProxies: []string{"10.0.0.0/8"}})
	require.NoError(t, err)
	assert.Equal(t, Result{"", SourceNone}, r.Lookup(req, "81.12.1.1", "10.1.1.1"))
}

func TestLookup_Overrides(t *testing.T) {
	r, err := New(Options{Overrides: map[string]string{"192.168.0.0/16": "ir", "203.0.113.7": "TR"}})
	require.NoError(t, err)
	assert.Equal(t, Result{"IR", SourceOverride}, r.Lookup(nil, "192.168.5.5", "192.168.5.5"))
	assert.Equal(t, Result{"TR", SourceOverride}, r.Lookup(nil, "203.0.113.7", "203.0.113.7"))
	assert.Equal(t, Result{"", SourceNone}, r.Lookup(nil, "203.0.113.8", "203.0.113.8"))

	var nilResolver *Resolver
	assert.Equal(t, Result{"", SourceNone}, nilResolver.Lookup(nil, "8.8.8.8", "8.8.8.8"))
	assert.NoError(t, nilResolver.Load())
	assert.NoError(t, nilResolver.Check(time.Now()))
}

func TestNew_RefusesInvalidOptions(t *testing.T) {
	for name, opts := range map[string]Options{
		"proxy":            {TrustedProxies: []string{"10.0.0.0/33"}},
		"override network": {Overrides: map[string]string{"nope": "IR"}},
		"override country": {Overrides: map[string]string{"10.0.0.0/8": "Iran"}},
	} {
		_, err := New(opts)
		assert.Error(t, err, name)
	}
}

func TestLoad_KeepsTheDatabaseWhenTheNewFileIsBroken(t *testing.T) {
	path := newDatabase(t)
	r, err := New(Options{DatabasePath: path})
	require.NoError(t, err)
	require.NoError(t, r.Load())

	require.NoError(t, os.WriteFile(path, []byte("not a database"), 0o600))
	assert.Error(t, r.Load())
	assert.Equal(t, "IR", r.Lookup(nil, "81.12.1.1", "81.12.1.1").Country, "the loaded database keeps answering")
	assert.Error(t, r.Check(time.Now()), "the failed reload is reported")

	// A database that is not about countries is refused too.
	writeDatabase(t, path, "GeoLite2-ASN", time.Now(), map[string]string{"81.12.0.0/16": "IR"})
	assert.ErrorContains(t, r.Load(), `"GeoLite2-ASN"`)

	writeDatabase(t, path, "DBIP-Country-Lite", time.Now(), map[string]string{"81.12.0.0/16": "AE"})
	require.NoError(t, r.Load())
	assert.Equal(t, "AE", r.Lookup(nil, "81.12.1.1", "81.12.1.1").Country)
	assert.NoError(t, r.Check(time.Now()))

	missing, err := New(Options{DatabasePath: filepath.Join(t.TempDir(), "missing.mmdb")})
	require.NoError(t, err)
	assert.Error(t, missing.Load())
	assert.Error(t, missing.Check(time.Now()))
}

func TestCheck_ReportsAStaleDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.mmdb")
	built := time.Now().Add(-60 * 24 * time.Hour)
	writeDatabase(t, path, "GeoLite2-Country", built, testNetworks)
	r, err := New(Options{DatabasePath: path})
	require.NoError(t, err)
	require.NoError(t, r.Load())
	assert.ErrorIs(t, r.Check(time.Now()), ErrStale)
	assert.NoError(t, r.Check(built.Add(time.Hour)))

	r, err = New(Options{DatabasePath: path, MaxAge: 90 * 24 * time.Hour})
	require.NoError(t, err)
	require.NoError(t, r.Load())
	assert.NoError(t, r.Check(time.Now()))
}

func TestWatch_ReloadsAChangedFile(t *testing.T) {
	path := newDatabase(t)
	r, err := New(Options{DatabasePath: path})
	require.NoError(t, err)
	require.NoError(t, r.Load())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		r.Watch(ctx, 10*time.Millisecond)
		close(done)
	}()

	writeDatabase(t, path, "GeoLite2-Country", time.Now(), map[string]string{"81.12.0.0/16": "TR", "8.8.8.0/24": "US", "1.0.0.0/8": "AU"})
	assert.Eventually(t, func() bool {
		return r.Lookup(nil, "81.12.1.1", "81.12.1.1").Country == "TR"
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	<-done
}

// TestLookup_ConcurrentWithReloads looks up while the file is replaced,
// for the race detector.
func TestLookup_ConcurrentWithReloads(t *testing.T) {
	path := newDatabase(t)
	r, err := New(Options{DatabasePath: path})
	require.NoError(t, err)
	require.NoError(t, r.Load())
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-stop:
				return
			default:
				country := r.Lookup(nil, "81.12.1.1", "81.12.1.1").Country
				assert.Contains(t, []string{"IR", "TR"}, country)
			}
		}
	}()
	for i := 0; i < 20; i++ {
		country := "IR"
		if i%2 == 0 {
			country = "TR"
		}
		writeDatabase(t, path, "GeoLite2-Country", time.Now(), map[string]string{"81.12.0.0/16": country})
		require.NoError(t, r.Load())
	}
	close(stop)
	<-finished
}

func TestResolve_LooksUpOncePerRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r, err := New(Options{Overrides: map[string]string{"192.0.2.0/24": "IR"}})
	require.NoError(t, err)
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies(nil))
	engine.GET("/", func(c *gin.Context) {
		first := r.Resolve(c)
		r.overrides = nil // a second lookup would find nothing
		assert.Equal(t, first, r.Resolve(c))
		c.String(http.StatusOK, first.Country)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	assert.Equal(t, "IR", w.Body.String())
}

func TestNormalizeCountryAndPublic(t *testing.T) {
	for in, want := range map[string]string{"ir": "IR", " IR ": "IR", "XK": "XK"} {
		got, ok := NormalizeCountry(in)
		assert.True(t, ok, in)
		assert.Equal(t, want, got)
	}
	for _, in := range []string{"", "X", "IRN", "T1", "A1", "XX", "ZZ", "EU", "AP", "I-"} {
		_, ok := NormalizeCountry(in)
		assert.False(t, ok, in)
	}
	assert.True(t, Public(netip.MustParseAddr("8.8.8.8")))
	assert.False(t, Public(netip.MustParseAddr("0.0.0.0")))
	assert.False(t, Public(netip.MustParseAddr("224.0.0.1")))
	assert.False(t, Public(netip.Addr{}))
}
