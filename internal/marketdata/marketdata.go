// Package marketdata keeps the global market data catalog (#262): the
// assets that external sources list, the exchanges (venues) whose markets
// users compare, their pairs and latest tickers, and the runs of the jobs
// that collect them. cmd/market-worker runs the jobs.
//
// The catalog is separate from the currencies listed on this exchange. Its
// assets are unverified: nothing here makes an asset tradable, and a link
// to a listed currency changes nothing on the currency.
//
// Several worker nodes may write at once, so every write of the Store is
// idempotent and safe to race: inserts use ON CONFLICT, an update changes a
// row only when a value differs, and a ticker replaces only an older one.
package marketdata

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// Status of an asset of the catalog.
const (
	// StatusUnverified is an asset that a source lists; it is served with
	// an "unverified" label.
	StatusUnverified = "unverified"
	// StatusHidden is an asset that an administrator hid; it is never
	// served.
	StatusHidden = "hidden"
	// StatusLinked is an asset that an administrator linked to a listed
	// currency.
	StatusLinked = "linked"
)

// Kind of an asset.
const (
	KindCoin    = "coin"
	KindToken   = "token"
	KindUnknown = "unknown"
)

// Kind of a venue.
const (
	VenueCEX = "cex"
	VenueDEX = "dex"
)

// Region of a venue: the market it serves. A release for users outside Iran
// leaves the Iranian venues out (#272).
const (
	RegionGlobal = "global"
	RegionIran   = "ir"
)

// Mode of a job run.
const (
	// ModeLive writes what a job collects.
	ModeLive = "live"
	// ModeDryRun collects and checks, and writes only the record of the run,
	// so that a new source or job can be tried in production first.
	ModeDryRun = "dry_run"
)

// Asset is an asset of the catalog.
type Asset struct {
	ID     int64
	Slug   string
	Symbol string
	Name   string
	Kind   string
	Status string
	// CurrencyHK is the listed currency of a linked asset.
	CurrencyHK *string
	Active     bool
	// Description is plain text.
	Description string
	// Links holds https links by kind, such as "website".
	Links             map[string]string
	LogoURL           string
	MarketCapRank     *int
	CirculatingSupply *shared.Decimal
	TotalSupply       *shared.Decimal
	MaxSupply         *shared.Decimal
	Attribution       string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	LastSeenAt        time.Time
}

// AssetDetails are the fields of an asset that a source provides. A job
// writes them with Store.UpdateAsset.
type AssetDetails struct {
	Symbol            string
	Name              string
	Kind              string
	Description       string
	Links             map[string]string
	LogoURL           string
	MarketCapRank     *int
	CirculatingSupply *shared.Decimal
	TotalSupply       *shared.Decimal
	MaxSupply         *shared.Decimal
	Attribution       string
}

// Venue is an exchange whose markets the catalog compares.
type Venue struct {
	ID          string
	Name        string
	Kind        string
	Region      string
	Country     string
	Website     string
	Attribution string
	Enabled     bool
}

// VenuePair is a pair at a venue.
type VenuePair struct {
	ID           int64
	VenueID      string
	VenueSymbol  string
	BaseSymbol   string
	QuoteSymbol  string
	BaseAssetID  *int64
	QuoteAssetID *int64
	Active       bool
}

// Ticker is the latest ticker of a pair. The figures other than the last
// price are optional, since not every venue reports them.
type Ticker struct {
	PairID      int64
	LastPrice   shared.Decimal
	Bid         *shared.Decimal
	Ask         *shared.Decimal
	VolumeBase  *shared.Decimal
	VolumeQuote *shared.Decimal
	High        *shared.Decimal
	Low         *shared.Decimal
	Open        *shared.Decimal
	ObservedAt  time.Time
	FetchedAt   time.Time
}

// Run is the record of a job run.
type Run struct {
	ID           int64
	Job          string
	Node         string
	Mode         string
	Status       string
	StartedAt    time.Time
	FinishedAt   *time.Time
	ItemsRead    int
	ItemsWritten int
	Error        string
}

var (
	sourcePattern = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)
	evmAddress    = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	nonSlug       = regexp.MustCompile(`[^a-z0-9]+`)
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("marketdata: not found")

// Slugify turns a name or a source id into a slug: lower-case letters and
// digits, with single hyphens between words. It returns "" when nothing is
// left.
func Slugify(s string) string {
	s = nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 100 {
		s = strings.TrimRight(s[:100], "-")
	}
	return s
}

// NormalizeSymbol trims a symbol and puts it in upper case, as the catalog
// stores every symbol.
func NormalizeSymbol(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}

// NormalizeAddress returns a contract address as the catalog stores it:
// lower case for the hex addresses of EVM networks, whose case is only a
// checksum, and as it is for every other network, where case matters.
func NormalizeAddress(address string) string {
	address = strings.TrimSpace(address)
	if evmAddress.MatchString(address) {
		return strings.ToLower(address)
	}
	return address
}
