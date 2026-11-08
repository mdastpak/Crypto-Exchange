package marketdata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// VenueEnabled reports whether the jobs collect a venue, and ErrNotFound
// for a venue that is not in the catalog.
func (s *Store) VenueEnabled(ctx context.Context, id string) (bool, error) {
	var enabled bool
	err := s.db.QueryRowContext(ctx, `SELECT enabled FROM market_venues WHERE id = $1`, id).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return enabled, err
}

// VenuePairs returns the pairs of a venue by venue symbol.
func (s *Store) VenuePairs(ctx context.Context, venueID string) (map[string]VenuePair, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, venue_id, venue_symbol, base_symbol, quote_symbol, base_asset_id, quote_asset_id, active
		FROM market_venue_pairs WHERE venue_id = $1`, venueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pairs := map[string]VenuePair{}
	for rows.Next() {
		var p VenuePair
		if err := rows.Scan(&p.ID, &p.VenueID, &p.VenueSymbol, &p.BaseSymbol, &p.QuoteSymbol,
			&p.BaseAssetID, &p.QuoteAssetID, &p.Active); err != nil {
			return nil, err
		}
		pairs[p.VenueSymbol] = p
	}
	return pairs, rows.Err()
}

// SymbolCandidate is an asset of the catalog that a symbol may name.
type SymbolCandidate struct {
	ID   int64
	Rank *int
}

// SymbolCandidates returns, for each symbol, the assets of the catalog
// with that symbol that are not hidden, the best-ranked first.
func (s *Store) SymbolCandidates(ctx context.Context, symbols []string) (map[string][]SymbolCandidate, error) {
	normalized := make([]string, len(symbols))
	for i, sym := range symbols {
		normalized[i] = NormalizeSymbol(sym)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT symbol, id, market_cap_rank FROM market_assets
		WHERE symbol = ANY($1::text[]) AND status <> 'hidden'
		ORDER BY symbol, market_cap_rank NULLS LAST, id`, normalized)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]SymbolCandidate{}
	for rows.Next() {
		var sym string
		var c SymbolCandidate
		if err := rows.Scan(&sym, &c.ID, &c.Rank); err != nil {
			return nil, err
		}
		out[sym] = append(out[sym], c)
	}
	return out, rows.Err()
}

// UpsertVenuePairs adds or updates the pairs of a venue in one statement,
// marks them active and seen, and returns their ids by venue symbol. A pair
// keeps the assets it is mapped to: an asset id of pairs only fills a
// missing one, so that the mapping of an administrator stays.
func (s *Store) UpsertVenuePairs(ctx context.Context, venueID string, pairs []VenuePair) (map[string]int64, error) {
	if len(pairs) == 0 {
		return map[string]int64{}, nil
	}
	symbols := make([]string, len(pairs))
	bases := make([]string, len(pairs))
	quotes := make([]string, len(pairs))
	baseIDs := make([]*int64, len(pairs))
	quoteIDs := make([]*int64, len(pairs))
	for i, p := range pairs {
		symbols[i], bases[i], quotes[i] = p.VenueSymbol, NormalizeSymbol(p.BaseSymbol), NormalizeSymbol(p.QuoteSymbol)
		if symbols[i] == "" || bases[i] == "" || quotes[i] == "" {
			return nil, errors.New("marketdata: a pair needs its venue symbol, base, and quote")
		}
		baseIDs[i], quoteIDs[i] = p.BaseAssetID, p.QuoteAssetID
	}
	rows, err := s.db.QueryContext(ctx, `
		INSERT INTO market_venue_pairs (venue_id, venue_symbol, base_symbol, quote_symbol, base_asset_id, quote_asset_id)
		SELECT $1, p.venue_symbol, p.base_symbol, p.quote_symbol, p.base_asset_id, p.quote_asset_id
		FROM unnest($2::text[], $3::text[], $4::text[], $5::bigint[], $6::bigint[])
			AS p(venue_symbol, base_symbol, quote_symbol, base_asset_id, quote_asset_id)
		ON CONFLICT (venue_id, venue_symbol) DO UPDATE SET
			base_symbol = excluded.base_symbol, quote_symbol = excluded.quote_symbol,
			base_asset_id = COALESCE(market_venue_pairs.base_asset_id, excluded.base_asset_id),
			quote_asset_id = COALESCE(market_venue_pairs.quote_asset_id, excluded.quote_asset_id),
			active = TRUE, updated_at = NOW(), last_seen_at = GREATEST(market_venue_pairs.last_seen_at, NOW())
		RETURNING venue_symbol, id`,
		venueID, symbols, bases, quotes, baseIDs, quoteIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string]int64, len(pairs))
	for rows.Next() {
		var sym string
		var id int64
		if err := rows.Scan(&sym, &id); err != nil {
			return nil, err
		}
		ids[sym] = id
	}
	return ids, rows.Err()
}

// UpsertTickers writes the latest tickers of pairs in one statement, each
// only over an older observation, as UpsertTicker does, and returns how
// many it wrote.
func (s *Store) UpsertTickers(ctx context.Context, tickers []Ticker) (int, error) {
	if len(tickers) == 0 {
		return 0, nil
	}
	n := len(tickers)
	pairIDs := make([]int64, n)
	last := make([]string, n)
	bid, ask, volBase, volQuote, high, low, open := make([]*string, n), make([]*string, n), make([]*string, n),
		make([]*string, n), make([]*string, n), make([]*string, n), make([]*string, n)
	observed, fetched := make([]time.Time, n), make([]time.Time, n)
	text := func(d *shared.Decimal) *string {
		if d == nil || d.IsNegative() {
			return nil
		}
		s := d.String()
		return &s
	}
	for i, t := range tickers {
		if t.LastPrice.IsNegative() {
			return 0, errors.New("marketdata: a negative price")
		}
		if t.ObservedAt.IsZero() || t.FetchedAt.IsZero() {
			return 0, errors.New("marketdata: a ticker needs its observed and fetched times")
		}
		pairIDs[i], last[i] = t.PairID, t.LastPrice.String()
		bid[i], ask[i], volBase[i], volQuote[i] = text(t.Bid), text(t.Ask), text(t.VolumeBase), text(t.VolumeQuote)
		high[i], low[i], open[i] = text(t.High), text(t.Low), text(t.Open)
		observed[i], fetched[i] = t.ObservedAt.UTC(), t.FetchedAt.UTC()
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO market_tickers_latest (pair_id, last_price, bid, ask, volume_base, volume_quote,
			high, low, open_price, observed_at, fetched_at)
		SELECT * FROM unnest($1::bigint[], $2::numeric[], $3::numeric[], $4::numeric[], $5::numeric[],
			$6::numeric[], $7::numeric[], $8::numeric[], $9::numeric[], $10::timestamptz[], $11::timestamptz[])
		ON CONFLICT (pair_id) DO UPDATE SET last_price = excluded.last_price, bid = excluded.bid,
			ask = excluded.ask, volume_base = excluded.volume_base, volume_quote = excluded.volume_quote,
			high = excluded.high, low = excluded.low, open_price = excluded.open_price,
			observed_at = excluded.observed_at, fetched_at = excluded.fetched_at
		WHERE market_tickers_latest.observed_at < excluded.observed_at`,
		pairIDs, last, bid, ask, volBase, volQuote, high, low, open, observed, fetched)
	if err != nil {
		return 0, err
	}
	written, err := res.RowsAffected()
	return int(written), err
}

// DeactivateVenuePairs marks inactive the active pairs of a venue that
// were last seen before before, which the venue no longer lists, and
// returns how many.
func (s *Store) DeactivateVenuePairs(ctx context.Context, venueID string, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, errors.New("marketdata: no time to deactivate before")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE market_venue_pairs SET active = FALSE, updated_at = NOW()
		WHERE venue_id = $1 AND active AND last_seen_at < $2`, venueID, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Quote is the latest ticker of an asset at one venue, with the venue and
// the flags of its quality.
type Quote struct {
	VenueID     string
	VenueName   string
	Region      string
	PairID      int64
	VenueSymbol string
	QuoteSymbol string
	Ticker      Ticker
	// Stale: the ticker is older than the staleness limit.
	Stale bool
	// Outlier: the price is too far from the median of the other venues.
	Outlier bool
	// Median is the median price of the fresh tickers of the same quote
	// symbol, when enough venues have one.
	Median *shared.Decimal
}

// QuotesOf returns the latest tickers of an asset as the base of the
// active pairs of the enabled venues of the regions, without their flags,
// which Flag sets. No region is every region.
func (s *Store) QuotesOf(ctx context.Context, assetID int64, regions []string) ([]Quote, error) {
	if regions == nil {
		regions = []string{}
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT v.id, v.name, v.region, p.id, p.venue_symbol, p.quote_symbol,
			t.last_price, t.bid, t.ask, t.volume_base, t.volume_quote, t.high, t.low, t.open_price,
			t.observed_at, t.fetched_at
		FROM market_venue_pairs p
		JOIN market_venues v ON v.id = p.venue_id
		JOIN market_tickers_latest t ON t.pair_id = p.id
		WHERE p.base_asset_id = $1 AND p.active AND v.enabled
			AND (cardinality($2::text[]) = 0 OR v.region = ANY($2::text[]))
		ORDER BY p.quote_symbol, v.id, p.venue_symbol`, assetID, regions)
	if err != nil {
		return nil, fmt.Errorf("marketdata: quotes of asset %d: %w", assetID, err)
	}
	defer rows.Close()
	var out []Quote
	for rows.Next() {
		var q Quote
		t := &q.Ticker
		if err := rows.Scan(&q.VenueID, &q.VenueName, &q.Region, &q.PairID, &q.VenueSymbol, &q.QuoteSymbol,
			&t.LastPrice, &t.Bid, &t.Ask, &t.VolumeBase, &t.VolumeQuote, &t.High, &t.Low, &t.Open,
			&t.ObservedAt, &t.FetchedAt); err != nil {
			return nil, err
		}
		t.PairID = q.PairID
		out = append(out, q)
	}
	return out, rows.Err()
}
