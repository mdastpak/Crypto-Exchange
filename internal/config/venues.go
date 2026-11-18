package config

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/shopspring/decimal"
)

// The connectors of the external exchanges (#384), by their name in
// venues.exchanges.
const (
	VenueBinance = "binance"
	VenueNobitex = "nobitex"
)

// KnownVenues are the venues that have a connector.
var KnownVenues = []string{VenueBinance, VenueNobitex}

// IranianVenues are the venues that only a release of the ir region may turn
// on (#272).
var IranianVenues = []string{VenueNobitex}

// IsKnownVenue reports whether name is one of KnownVenues. It names the
// constants rather than read the variable, so that any package may call it
// without reading the global state (#48); a test keeps the two alike.
func IsKnownVenue(name string) bool { return name == VenueBinance || name == VenueNobitex }

// IsIranianVenue reports whether name is one of IranianVenues, which only
// a release of the ir region may use (#272).
func IsIranianVenue(name string) bool { return name == VenueNobitex }

// The modes of a venue, as internal/venues names them.
const (
	VenueModeTestnet = "testnet"
	VenueModeLive    = "live"
	VenueModeDryRun  = "dry_run"
)

// The providers of the credentials of the venues.
const (
	VenueCredentialsOpenBao = "openbao"
	VenueCredentialsFile    = "file"
)

// The targets of a withdrawal from a venue (#383, #388).
const (
	// VenueWithdrawOwnWallet sends a withdrawal to our hot wallet, which
	// pays the user, so that the venue never sees a user's address.
	VenueWithdrawOwnWallet = "own_wallet"
	// VenueWithdrawDirect sends it from the venue to the user's address.
	VenueWithdrawDirect = "direct"
)

// The bounds and defaults of the venues.
const (
	DefaultVenueTimeoutSeconds            = 10
	MaxVenueTimeoutSeconds                = 60
	DefaultVenueCredentialsCacheSeconds   = 300
	MaxVenueCredentialsCacheSeconds       = 3600
	DefaultVenueRecvWindowMs              = 5000
	MaxVenueRecvWindowMs                  = 60000
	DefaultVenueOpenBaoMount              = "secret"
	DefaultVenueOpenBaoPrefix             = "venues/"
	DefaultVenueOpenBaoTimeoutSeconds     = 10
	DefaultVenueRequestsPerMinuteBinance  = 600
	DefaultVenueRequestsPerMinuteNobitex  = 60
	defaultVenueRequestsPerMinuteFallback = 60
	DefaultVenueStateIntervalSeconds      = 60
	MinVenueStateIntervalSeconds          = 10
	DefaultVenueStateMaxAgeSeconds        = 300
	DefaultVenueStateConfirmRuns          = 2
	MaxVenueStateConfirmRuns              = 10
	// MaxVenueCostBPS bounds the fee and the slippage of a venue.
	MaxVenueCostBPS = 1000
)

// VenuesConfig configures the connectors of the external exchanges (#384).
// See docs/venues.md and docs/configuration.md#venues.
type VenuesConfig struct {
	// ProxyURL is the proxy of the venues whose egress is proxy: http://,
	// https://, or socks5://. Set it with VENUES_PROXY_URL; it is never
	// logged.
	ProxyURL    string                 `yaml:"proxy_url"`
	Credentials VenueCredentialsConfig `yaml:"credentials"`
	// Exchanges are the venues by the name of their connector.
	Exchanges map[string]VenueConfig `yaml:"exchanges"`
	// State configures the snapshots and the reconciliation of the venues
	// (#385).
	State VenueStateConfig `yaml:"state"`
}

// VenueStateConfig configures the state of the venues (#385): how often a
// job of cmd/market-worker takes the snapshots of each enabled venue and
// reconciles it with the books, how old a snapshot may be before the router
// treats it as unknown, and the differences that a reconciliation accepts.
type VenueStateConfig struct {
	IntervalSeconds int `yaml:"interval_seconds"`
	MaxAgeSeconds   int `yaml:"max_age_seconds"`
	// ConfirmRuns is how many reconciliations in a row must find a
	// difference before the currency is held.
	ConfirmRuns int `yaml:"confirm_runs"`
	// Tolerances are the accepted differences, by our currency symbol, as
	// decimals; a currency that is not here accepts none.
	Tolerances map[string]string `yaml:"tolerances"`
}

// VenueCredentialsConfig says where the API keys of the venues are read.
type VenueCredentialsConfig struct {
	// Provider is openbao, or file for development; release mode refuses
	// file.
	Provider string `yaml:"provider"`
	// File is the JSON file of the file provider.
	File string `yaml:"file"`
	// CacheSeconds is how long credentials are kept before they are read
	// again, so that a rotation applies without a restart.
	CacheSeconds int                `yaml:"cache_seconds"`
	OpenBao      VenueOpenBaoConfig `yaml:"openbao"`
}

// VenueOpenBaoConfig locates the secrets in the KV version 2 engine of
// OpenBao or Vault: one per venue, at <mount>/data/<prefix><venue>.
type VenueOpenBaoConfig struct {
	Address string `yaml:"address"`
	// Token is set with VENUES_OPENBAO_TOKEN; TokenFile, when set, is read
	// on every request, so that an agent can renew it in place.
	Token          string `yaml:"token"`
	TokenFile      string `yaml:"token_file"`
	Mount          string `yaml:"mount"`
	Prefix         string `yaml:"prefix"`
	CACert         string `yaml:"ca_cert"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// VenueConfig configures one venue.
type VenueConfig struct {
	Enabled bool `yaml:"enabled"`
	// Mode is testnet, live, or dry_run: dry_run reads the live venue and
	// logs the orders, withdrawals, and transfers without sending them.
	Mode string `yaml:"mode"`
	// Egress is direct, or proxy through venues.proxy_url.
	Egress string `yaml:"egress"`
	// BaseURL overrides the address of the mode.
	BaseURL string `yaml:"base_url"`
	// RequestsPerMinute is our share of the venue's limit, counted across
	// every node; 0 takes the default of the venue.
	RequestsPerMinute int `yaml:"requests_per_minute"`
	TimeoutSeconds    int `yaml:"timeout_seconds"`
	// RecvWindowMs is how long after its timestamp Binance accepts a
	// signed request.
	RecvWindowMs int `yaml:"recv_window_ms"`
	// SubAccount is the sub-account that the OTC trades use, such as the
	// email of a Binance sub-account; empty is the main account.
	SubAccount string `yaml:"sub_account"`
	// WithdrawTarget is own_wallet (the default) or direct.
	WithdrawTarget string `yaml:"withdraw_target"`
	// Assets map the venue's assets, by its symbol, to our currencies and
	// networks. An asset that is not here is not routable.
	Assets map[string]VenueAsset `yaml:"assets"`
	// FeeBPS is the taker fee of our account at the venue, and SlippageBPS
	// the slippage that the routing rules expect of an order there, both
	// in basis points of the notional: the cost of a leg routed there
	// (#386) is their sum and the withdrawal fee of the network.
	FeeBPS      int `yaml:"fee_bps"`
	SlippageBPS int `yaml:"slippage_bps"`
}

// VenueAsset maps an asset of a venue to our currency, and its networks, by
// the venue's name, to ours.
type VenueAsset struct {
	Currency string            `yaml:"currency"`
	Networks map[string]string `yaml:"networks"`
}

// DefaultVenueRequestsPerMinute is the share of a venue's limit that a
// venue takes when it sets none: well under the documented limit, since the
// market data service may call the same venue from the same address.
func DefaultVenueRequestsPerMinute(name string) int {
	switch name {
	case VenueBinance:
		return DefaultVenueRequestsPerMinuteBinance
	case VenueNobitex:
		return DefaultVenueRequestsPerMinuteNobitex
	}
	return defaultVenueRequestsPerMinuteFallback
}

// applyVenueDefaults fills what the venues leave unset.
func (c *Configuration) applyVenueDefaults() {
	v := &c.Venues
	if v.Credentials.CacheSeconds == 0 {
		v.Credentials.CacheSeconds = DefaultVenueCredentialsCacheSeconds
	}
	st := &v.State
	if st.IntervalSeconds == 0 {
		st.IntervalSeconds = DefaultVenueStateIntervalSeconds
	}
	if st.MaxAgeSeconds == 0 {
		st.MaxAgeSeconds = DefaultVenueStateMaxAgeSeconds
	}
	if st.ConfirmRuns == 0 {
		st.ConfirmRuns = DefaultVenueStateConfirmRuns
	}
	b := &v.Credentials.OpenBao
	if b.Mount == "" {
		b.Mount = DefaultVenueOpenBaoMount
	}
	if b.Prefix == "" {
		b.Prefix = DefaultVenueOpenBaoPrefix
	}
	if b.TimeoutSeconds == 0 {
		b.TimeoutSeconds = DefaultVenueOpenBaoTimeoutSeconds
	}
	for name, e := range v.Exchanges {
		if e.Mode == "" {
			e.Mode = VenueModeTestnet
		}
		if e.Egress == "" {
			e.Egress = EgressDirect
		}
		if e.RequestsPerMinute == 0 {
			e.RequestsPerMinute = DefaultVenueRequestsPerMinute(name)
		}
		if e.TimeoutSeconds == 0 {
			e.TimeoutSeconds = DefaultVenueTimeoutSeconds
		}
		if e.RecvWindowMs == 0 {
			e.RecvWindowMs = DefaultVenueRecvWindowMs
		}
		if e.WithdrawTarget == "" {
			e.WithdrawTarget = VenueWithdrawOwnWallet
		}
		v.Exchanges[name] = e
	}
}

// EnabledVenues are the names of the enabled venues, in order.
func (v VenuesConfig) EnabledVenues() []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(v.Exchanges)) {
		if v.Exchanges[name].Enabled {
			out = append(out, name)
		}
	}
	return out
}

var venueAssetSymbol = regexp.MustCompile(`^[A-Za-z0-9]{1,20}$`)

// venueProblems checks the venues section.
func (c *Configuration) venueProblems(p *problems, release bool) {
	v := c.Venues
	if v.ProxyURL != "" {
		u, err := url.Parse(v.ProxyURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
			// The value may hold credentials, so it is not repeated.
			p.add("venues.proxy_url", "is not an http://, https://, or socks5:// address with a host")
		}
	}
	enabled := v.EnabledVenues()
	for _, name := range slices.Sorted(maps.Keys(v.Exchanges)) {
		venueConfigProblems(p, name, v.Exchanges[name], v.ProxyURL, release)
	}
	if len(enabled) > 0 {
		venueStateProblems(p, v.State)
		c.venueCredentialProblems(p, release)
	}
}

func venueConfigProblems(p *problems, name string, e VenueConfig, proxyURL string, release bool) {
	key := "venues.exchanges." + name
	if !slices.Contains(KnownVenues, name) {
		p.add(key, "is not a venue with a connector; it must be one of %s", strings.Join(KnownVenues, ", "))
		return
	}
	switch e.Mode {
	case VenueModeTestnet, VenueModeLive, VenueModeDryRun:
	default:
		p.add(key+".mode", "is %q; it must be testnet, live, or dry_run", e.Mode)
	}
	if name == VenueNobitex && e.Mode == VenueModeTestnet && e.Enabled {
		p.add(key+".mode", "is testnet, but Nobitex has no testnet: set live, or dry_run to read it without sending orders")
	}
	switch e.Egress {
	case EgressDirect:
	case EgressProxy:
		if e.Enabled && proxyURL == "" {
			p.add(key+".egress", "is proxy, but venues.proxy_url (VENUES_PROXY_URL) is not set")
		}
	default:
		p.add(key+".egress", "is %q; it must be direct or proxy", e.Egress)
	}
	if e.BaseURL != "" {
		u, err := url.Parse(e.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && (release || u.Scheme != "http")) {
			p.add(key+".base_url", "is %q; it must be an https:// address", e.BaseURL)
		}
	}
	if e.RequestsPerMinute < 1 {
		p.add(key+".requests_per_minute", "is %d; it must be at least 1", e.RequestsPerMinute)
	}
	if e.FeeBPS < 0 || e.FeeBPS > MaxVenueCostBPS {
		p.add(key+".fee_bps", "is %d; it must be from 0 to %d", e.FeeBPS, MaxVenueCostBPS)
	}
	if e.SlippageBPS < 0 || e.SlippageBPS > MaxVenueCostBPS {
		p.add(key+".slippage_bps", "is %d; it must be from 0 to %d", e.SlippageBPS, MaxVenueCostBPS)
	}
	if e.TimeoutSeconds < 1 || e.TimeoutSeconds > MaxVenueTimeoutSeconds {
		p.add(key+".timeout_seconds", "is %d; it must be from 1 to %d", e.TimeoutSeconds, MaxVenueTimeoutSeconds)
	}
	if e.RecvWindowMs < 1 || e.RecvWindowMs > MaxVenueRecvWindowMs {
		p.add(key+".recv_window_ms", "is %d; it must be from 1 to %d", e.RecvWindowMs, MaxVenueRecvWindowMs)
	}
	if e.WithdrawTarget != VenueWithdrawOwnWallet && e.WithdrawTarget != VenueWithdrawDirect {
		p.add(key+".withdraw_target", "is %q; it must be own_wallet or direct", e.WithdrawTarget)
	}
	if e.SubAccount != "" && name == VenueNobitex {
		p.add(key+".sub_account", "is set, but Nobitex has no sub-accounts")
	}
	venueAssetProblems(p, key+".assets", e.Assets)
}

// venueAssetProblems refuses a mapping that maps two assets to one
// currency, or two networks of an asset to one network, so that each of
// our currencies and networks has one asset at the venue.
func venueAssetProblems(p *problems, key string, assets map[string]VenueAsset) {
	byCurrency := map[string]string{}
	for _, asset := range slices.Sorted(maps.Keys(assets)) {
		a := assets[asset]
		akey := key + "." + asset
		if !venueAssetSymbol.MatchString(asset) {
			p.add(akey, "is not an asset symbol: letters and digits")
		}
		if a.Currency == "" {
			p.add(akey+".currency", "is not set")
			continue
		}
		currency := strings.ToUpper(a.Currency)
		if other, ok := byCurrency[currency]; ok {
			p.add(akey+".currency", "is %s, which %s maps to as well", a.Currency, other)
		}
		byCurrency[currency] = asset
		byNetwork := map[string]string{}
		for _, vn := range slices.Sorted(maps.Keys(a.Networks)) {
			n := a.Networks[vn]
			if n == "" {
				p.add(akey+".networks."+vn, "maps to no network")
				continue
			}
			if other, ok := byNetwork[n]; ok {
				p.add(akey+".networks."+vn, "is %s, which %s maps to as well", n, other)
			}
			byNetwork[n] = vn
		}
	}
}

func (c *Configuration) venueCredentialProblems(p *problems, release bool) {
	cr := c.Venues.Credentials
	switch cr.Provider {
	case VenueCredentialsFile:
		if release {
			p.add("venues.credentials.provider", "is file; release mode reads the keys of the venues from OpenBao: set it to openbao")
		}
		if cr.File == "" {
			p.add("venues.credentials.file", "is not set; the file provider needs the path of its JSON file")
		}
	case VenueCredentialsOpenBao:
		b := cr.OpenBao
		u, err := url.Parse(b.Address)
		switch {
		case b.Address == "" || err != nil || u.Host == "":
			p.add("venues.credentials.openbao.address", "is not an address of OpenBao, such as https://openbao.internal:8200")
		case release && u.Scheme != "https":
			p.add("venues.credentials.openbao.address", "is %q; release mode reaches OpenBao over https", b.Address)
		}
		if b.Token == "" && b.TokenFile == "" {
			p.add("venues.credentials.openbao.token", "and venues.credentials.openbao.token_file are not set; OpenBao needs one (VENUES_OPENBAO_TOKEN)")
		}
		if b.TimeoutSeconds < 1 || b.TimeoutSeconds > MaxVenueTimeoutSeconds {
			p.add("venues.credentials.openbao.timeout_seconds", "is %d; it must be from 1 to %d", b.TimeoutSeconds, MaxVenueTimeoutSeconds)
		}
	default:
		p.add("venues.credentials.provider", "is %q; it must be openbao, or file for development, while a venue is enabled", cr.Provider)
	}
	if cr.CacheSeconds < 1 || cr.CacheSeconds > MaxVenueCredentialsCacheSeconds {
		p.add("venues.credentials.cache_seconds", "is %d; it must be from 1 to %d", cr.CacheSeconds, MaxVenueCredentialsCacheSeconds)
	}
}

// venueRegionSettings are the enabled venues of IranianVenues.
func venueRegionSettings(c *Configuration) []regionSetting {
	var out []regionSetting
	for _, name := range c.Venues.EnabledVenues() {
		if slices.Contains(IranianVenues, name) {
			out = append(out, regionSetting{fmt.Sprintf("venues.exchanges.%s.enabled", name), "true", "set it to false"})
		}
	}
	return out
}

// venueStateProblems checks venues.state.
func venueStateProblems(p *problems, st VenueStateConfig) {
	if st.IntervalSeconds < MinVenueStateIntervalSeconds {
		p.add("venues.state.interval_seconds", "is %d; it must be at least %d", st.IntervalSeconds, MinVenueStateIntervalSeconds)
	}
	// A snapshot must live through at least two runs, or every run that
	// is a little late makes the venues not routable.
	if st.MaxAgeSeconds < 2*st.IntervalSeconds {
		p.add("venues.state.max_age_seconds", "is %d; it must be at least twice interval_seconds (%d)", st.MaxAgeSeconds, 2*st.IntervalSeconds)
	}
	if st.ConfirmRuns < 1 || st.ConfirmRuns > MaxVenueStateConfirmRuns {
		p.add("venues.state.confirm_runs", "is %d; it must be from 1 to %d", st.ConfirmRuns, MaxVenueStateConfirmRuns)
	}
	for _, symbol := range slices.Sorted(maps.Keys(st.Tolerances)) {
		d, err := decimal.NewFromString(st.Tolerances[symbol])
		if err != nil || d.IsNegative() {
			p.add("venues.state.tolerances."+symbol, "is not a decimal of zero or more")
		}
	}
}

// VenueTolerances are the tolerances of venues.state as decimals; the
// configuration check refused any that does not parse.
func (st VenueStateConfig) VenueTolerances() map[string]decimal.Decimal {
	out := make(map[string]decimal.Decimal, len(st.Tolerances))
	for symbol, raw := range st.Tolerances {
		if d, err := decimal.NewFromString(raw); err == nil {
			out[symbol] = d
		}
	}
	return out
}
