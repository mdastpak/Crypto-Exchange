package marketdata

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// maxSlugAttempts bounds the suffixes tried for a slug that another asset
// holds, such as "bitcoin-2".
const maxSlugAttempts = 20

// maxRunError bounds the error text kept with a run.
const maxRunError = 4000

// Store reads and writes the catalog in PostgreSQL.
type Store struct {
	db *sql.DB
}

// NewStore returns a store on db.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// CreateAsset adds an asset with its identity at a source and returns its
// id. The slug is made from slug, with a numeric suffix when another asset
// holds it. When the identity already exists, for example because another
// node created the asset a moment before, it adds nothing and returns the
// existing asset with created false.
func (s *Store) CreateAsset(ctx context.Context, slug, source, externalID string, details AssetDetails) (id int64, created bool, err error) {
	if err := checkIdentity(source, externalID); err != nil {
		return 0, false, err
	}
	base := Slugify(slug)
	if base == "" {
		return 0, false, fmt.Errorf("marketdata: %q gives an empty slug", slug)
	}
	details, err = details.normalized()
	if err != nil {
		return 0, false, err
	}
	links, err := json.Marshal(details.Links)
	if err != nil {
		return 0, false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()

	for attempt := 1; ; attempt++ {
		if attempt > maxSlugAttempts {
			return 0, false, fmt.Errorf("marketdata: no free slug for %q after %d attempts", base, maxSlugAttempts)
		}
		candidate := base
		if attempt > 1 {
			suffix := "-" + strconv.Itoa(attempt)
			candidate = strings.TrimRight(base[:min(len(base), 100-len(suffix))], "-") + suffix
		}
		err = tx.QueryRowContext(ctx, `
			INSERT INTO market_assets (slug, symbol, name, kind, description, links, logo_url,
				market_cap_rank, circulating_supply, total_supply, max_supply, attribution)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (slug) DO NOTHING
			RETURNING id`,
			candidate, details.Symbol, details.Name, details.Kind, details.Description, links, details.LogoURL,
			details.MarketCapRank, details.CirculatingSupply, details.TotalSupply, details.MaxSupply, details.Attribution,
		).Scan(&id)
		if err == nil {
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
	}

	// A node that inserts the same identity at the same time waits here
	// for the other transaction, and then finds the conflict.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO market_asset_sources (source, external_id, asset_id) VALUES ($1, $2, $3)
		ON CONFLICT (source, external_id) DO NOTHING`, source, externalID, id)
	if err != nil {
		return 0, false, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return 0, false, err
	} else if n == 0 {
		_ = tx.Rollback()
		existing, err := s.AssetBySource(ctx, source, externalID)
		return existing, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// LinkSource records that externalID at source is the asset id. It returns
// the asset that the identity belongs to, which is another asset when the
// identity was recorded for it before; the caller reports that conflict.
func (s *Store) LinkSource(ctx context.Context, assetID int64, source, externalID string) (owner int64, err error) {
	if err := checkIdentity(source, externalID); err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO market_asset_sources (source, external_id, asset_id) VALUES ($1, $2, $3)
		ON CONFLICT (source, external_id) DO NOTHING`, source, externalID, assetID); err != nil {
		return 0, err
	}
	return s.AssetBySource(ctx, source, externalID)
}

// AssetBySource returns the id of the asset whose identity at source is
// externalID, or ErrNotFound.
func (s *Store) AssetBySource(ctx context.Context, source, externalID string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT asset_id FROM market_asset_sources WHERE source = $1 AND external_id = $2`,
		source, externalID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// AddContract records the contract of a token on a network. It returns the
// asset that the contract belongs to, which is another asset when the
// contract was recorded for it before; the caller reports that conflict.
func (s *Store) AddContract(ctx context.Context, assetID int64, network, address, source string) (owner int64, err error) {
	network = strings.ToLower(strings.TrimSpace(network))
	address = NormalizeAddress(address)
	if network == "" || address == "" {
		return 0, errors.New("marketdata: a contract needs a network and an address")
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO market_asset_contracts (network, address, asset_id, source) VALUES ($1, $2, $3, $4)
		ON CONFLICT (network, address) DO NOTHING`, network, address, assetID, source); err != nil {
		return 0, err
	}
	return s.AssetByContract(ctx, network, address)
}

// AssetByContract returns the id of the asset with the contract, or
// ErrNotFound.
func (s *Store) AssetByContract(ctx context.Context, network, address string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT asset_id FROM market_asset_contracts WHERE network = $1 AND address = $2`,
		strings.ToLower(strings.TrimSpace(network)), NormalizeAddress(address)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// UpdateAsset writes the details of an asset and marks it active. It
// changes the row only when a value differs, so that a run that finds
// nothing new writes nothing, and reports whether it changed it. The status
// and the link, which administrators set, are left as they are.
func (s *Store) UpdateAsset(ctx context.Context, id int64, details AssetDetails) (changed bool, err error) {
	details, err = details.normalized()
	if err != nil {
		return false, err
	}
	links, err := json.Marshal(details.Links)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE market_assets SET symbol = $2, name = $3, kind = $4, description = $5, links = $6,
			logo_url = $7, market_cap_rank = $8, circulating_supply = $9, total_supply = $10,
			max_supply = $11, attribution = $12, active = TRUE, updated_at = NOW()
		WHERE id = $1 AND (symbol, name, kind, description, links, logo_url, market_cap_rank,
			circulating_supply, total_supply, max_supply, attribution, active)
			IS DISTINCT FROM ($2, $3, $4, $5, $6::jsonb, $7, $8::integer, $9::numeric, $10::numeric,
			$11::numeric, $12, TRUE)`,
		id, details.Symbol, details.Name, details.Kind, details.Description, links, details.LogoURL,
		details.MarketCapRank, details.CirculatingSupply, details.TotalSupply, details.MaxSupply, details.Attribution)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		var exists bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM market_assets WHERE id = $1)`, id).Scan(&exists); err != nil {
			return false, err
		}
		if !exists {
			return false, ErrNotFound
		}
	}
	return n > 0, nil
}

// MarkAssetsSeen records that a source listed the assets at seenAt. A later
// time never goes back.
func (s *Store) MarkAssetsSeen(ctx context.Context, ids []int64, seenAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE market_assets SET last_seen_at = $2
		WHERE id = ANY($1) AND last_seen_at < $2`, ids, seenAt)
	return err
}

// assetColumns are the columns of an Asset, in the order of scanAsset.
const assetColumns = `id, slug, symbol, name, kind, status, currency_hk::text, active, description, links,
	logo_url, market_cap_rank, circulating_supply, total_supply, max_supply, attribution,
	created_at, updated_at, last_seen_at`

// scanAsset reads an Asset from a row of assetColumns.
func scanAsset(row interface{ Scan(dest ...any) error }) (Asset, error) {
	var a Asset
	var links []byte
	if err := row.Scan(&a.ID, &a.Slug, &a.Symbol, &a.Name, &a.Kind, &a.Status, &a.CurrencyHK, &a.Active, &a.Description, &links,
		&a.LogoURL, &a.MarketCapRank, &a.CirculatingSupply, &a.TotalSupply, &a.MaxSupply, &a.Attribution,
		&a.CreatedAt, &a.UpdatedAt, &a.LastSeenAt); err != nil {
		return Asset{}, err
	}
	if err := json.Unmarshal(links, &a.Links); err != nil {
		return Asset{}, fmt.Errorf("marketdata: links of asset %d: %w", a.ID, err)
	}
	return a, nil
}

// Asset returns an asset by id, or ErrNotFound.
func (s *Store) Asset(ctx context.Context, id int64) (Asset, error) {
	a, err := scanAsset(s.db.QueryRowContext(ctx, `SELECT `+assetColumns+` FROM market_assets WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	return a, err
}

// UpsertVenue adds a venue or updates its description. Whether a venue is
// enabled is the administrators' choice: v.Enabled applies to a new venue
// only, and an existing venue keeps its setting.
func (s *Store) UpsertVenue(ctx context.Context, v Venue) error {
	if !sourcePattern.MatchString(v.ID) {
		return fmt.Errorf("marketdata: invalid venue id %q", v.ID)
	}
	if v.Kind != VenueCEX && v.Kind != VenueDEX {
		return fmt.Errorf("marketdata: invalid venue kind %q", v.Kind)
	}
	if v.Region != RegionGlobal && v.Region != RegionIran {
		return fmt.Errorf("marketdata: invalid venue region %q", v.Region)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO market_venues (id, name, kind, region, country, website, attribution, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, kind = excluded.kind,
			region = excluded.region, country = excluded.country, website = excluded.website,
			attribution = excluded.attribution, updated_at = NOW()
		WHERE (market_venues.name, market_venues.kind, market_venues.region, market_venues.country,
			market_venues.website, market_venues.attribution)
			IS DISTINCT FROM (excluded.name, excluded.kind, excluded.region, excluded.country,
			excluded.website, excluded.attribution)`,
		v.ID, v.Name, v.Kind, v.Region, strings.ToUpper(v.Country), v.Website, v.Attribution, v.Enabled)
	return err
}

// Venues returns every venue, ordered by id.
func (s *Store) Venues(ctx context.Context) ([]Venue, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, kind, region, country, website, attribution, enabled FROM market_venues ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var venues []Venue
	for rows.Next() {
		var v Venue
		if err := rows.Scan(&v.ID, &v.Name, &v.Kind, &v.Region, &v.Country, &v.Website, &v.Attribution, &v.Enabled); err != nil {
			return nil, err
		}
		venues = append(venues, v)
	}
	return venues, rows.Err()
}

// UpsertVenuePair adds a pair of a venue, or updates it, marks it active and
// seen, and returns its id. An asset id that p leaves nil keeps the mapping
// that the pair has.
func (s *Store) UpsertVenuePair(ctx context.Context, p VenuePair) (int64, error) {
	p.BaseSymbol, p.QuoteSymbol = NormalizeSymbol(p.BaseSymbol), NormalizeSymbol(p.QuoteSymbol)
	if p.VenueSymbol == "" || p.BaseSymbol == "" || p.QuoteSymbol == "" {
		return 0, errors.New("marketdata: a pair needs its venue symbol, base, and quote")
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO market_venue_pairs (venue_id, venue_symbol, base_symbol, quote_symbol, base_asset_id, quote_asset_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (venue_id, venue_symbol) DO UPDATE SET
			base_symbol = excluded.base_symbol, quote_symbol = excluded.quote_symbol,
			base_asset_id = COALESCE(excluded.base_asset_id, market_venue_pairs.base_asset_id),
			quote_asset_id = COALESCE(excluded.quote_asset_id, market_venue_pairs.quote_asset_id),
			active = TRUE, updated_at = NOW(), last_seen_at = GREATEST(market_venue_pairs.last_seen_at, NOW())
		RETURNING id`,
		p.VenueID, p.VenueSymbol, p.BaseSymbol, p.QuoteSymbol, p.BaseAssetID, p.QuoteAssetID).Scan(&id)
	return id, err
}

// UpsertTicker writes the latest ticker of a pair, unless the stored one was
// observed at the same time or later, so that a slower node or a late
// response never puts an older ticker back. It reports whether it wrote.
func (s *Store) UpsertTicker(ctx context.Context, t Ticker) (written bool, err error) {
	if t.LastPrice.IsNegative() {
		return false, errors.New("marketdata: a negative price")
	}
	if t.ObservedAt.IsZero() || t.FetchedAt.IsZero() {
		return false, errors.New("marketdata: a ticker needs its observed and fetched times")
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO market_tickers_latest (pair_id, last_price, bid, ask, volume_base, volume_quote,
			high, low, open_price, observed_at, fetched_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (pair_id) DO UPDATE SET last_price = excluded.last_price, bid = excluded.bid,
			ask = excluded.ask, volume_base = excluded.volume_base, volume_quote = excluded.volume_quote,
			high = excluded.high, low = excluded.low, open_price = excluded.open_price,
			observed_at = excluded.observed_at, fetched_at = excluded.fetched_at
		WHERE market_tickers_latest.observed_at < excluded.observed_at`,
		t.PairID, t.LastPrice, t.Bid, t.Ask, t.VolumeBase, t.VolumeQuote, t.High, t.Low, t.Open,
		t.ObservedAt.UTC(), t.FetchedAt.UTC())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// TickerOf returns the latest ticker of a pair, or ErrNotFound.
func (s *Store) TickerOf(ctx context.Context, pairID int64) (Ticker, error) {
	var t Ticker
	err := s.db.QueryRowContext(ctx, `
		SELECT pair_id, last_price, bid, ask, volume_base, volume_quote, high, low, open_price,
			observed_at, fetched_at
		FROM market_tickers_latest WHERE pair_id = $1`, pairID).Scan(
		&t.PairID, &t.LastPrice, &t.Bid, &t.Ask, &t.VolumeBase, &t.VolumeQuote, &t.High, &t.Low, &t.Open,
		&t.ObservedAt, &t.FetchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticker{}, ErrNotFound
	}
	return t, err
}

// StartRun records the start of a job run and returns its id.
func (s *Store) StartRun(ctx context.Context, job, node, mode string) (int64, error) {
	if mode != ModeLive && mode != ModeDryRun {
		return 0, fmt.Errorf("marketdata: invalid mode %q", mode)
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO market_sync_runs (job, node, mode) VALUES ($1, $2, $3) RETURNING id`,
		job, node, mode).Scan(&id)
	return id, err
}

// FinishRun records the end of a run: succeeded when runErr is nil, failed
// with its message otherwise. A run that has finished keeps its record.
func (s *Store) FinishRun(ctx context.Context, id int64, read, written int, runErr error) error {
	status, message := "succeeded", ""
	if runErr != nil {
		status, message = "failed", clip(runErr.Error(), maxRunError)
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE market_sync_runs SET status = $2, finished_at = NOW(), items_read = $3,
			items_written = $4, error = $5
		WHERE id = $1 AND status = 'running'`, id, status, read, written, message)
	return err
}

// Runs returns the latest runs of a job, newest first, at most limit.
func (s *Store) Runs(ctx context.Context, job string, limit int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job, node, mode, status, started_at, finished_at, items_read, items_written, error
		FROM market_sync_runs WHERE job = $1 ORDER BY started_at DESC, id DESC LIMIT $2`, job, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.Job, &r.Node, &r.Mode, &r.Status, &r.StartedAt, &r.FinishedAt,
			&r.ItemsRead, &r.ItemsWritten, &r.Error); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// normalized checks the details and returns them as the catalog stores them.
func (d AssetDetails) normalized() (AssetDetails, error) {
	d.Symbol = NormalizeSymbol(d.Symbol)
	d.Name = strings.TrimSpace(d.Name)
	if d.Symbol == "" || utf8.RuneCountInString(d.Symbol) > 32 {
		return d, fmt.Errorf("marketdata: invalid symbol %q", d.Symbol)
	}
	if d.Name == "" || utf8.RuneCountInString(d.Name) > 200 {
		return d, fmt.Errorf("marketdata: invalid name %q", d.Name)
	}
	switch d.Kind {
	case "":
		d.Kind = KindUnknown
	case KindCoin, KindToken, KindUnknown:
	default:
		return d, fmt.Errorf("marketdata: invalid kind %q", d.Kind)
	}
	if d.LogoURL != "" && !strings.HasPrefix(d.LogoURL, "https://") {
		return d, fmt.Errorf("marketdata: the logo URL %q is not https", d.LogoURL)
	}
	for kind, link := range d.Links {
		if !strings.HasPrefix(link, "https://") {
			return d, fmt.Errorf("marketdata: the %s link %q is not https", kind, link)
		}
	}
	if d.Links == nil {
		d.Links = map[string]string{}
	}
	if d.MarketCapRank != nil && *d.MarketCapRank <= 0 {
		return d, fmt.Errorf("marketdata: invalid rank %d", *d.MarketCapRank)
	}
	for _, supply := range []*shared.Decimal{d.CirculatingSupply, d.TotalSupply, d.MaxSupply} {
		if supply != nil && supply.IsNegative() {
			return d, errors.New("marketdata: a negative supply")
		}
	}
	d.Description = clip(d.Description, 20000)
	return d, nil
}

func checkIdentity(source, externalID string) error {
	if !sourcePattern.MatchString(source) {
		return fmt.Errorf("marketdata: invalid source %q", source)
	}
	if externalID == "" || len(externalID) > 200 {
		return fmt.Errorf("marketdata: invalid external id %q", externalID)
	}
	return nil
}

// clip cuts s to at most n characters.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
