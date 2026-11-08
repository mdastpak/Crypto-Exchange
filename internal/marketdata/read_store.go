package marketdata

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// unrankedOrder sorts the assets without a rank after the ranked ones, in
// the public list.
const unrankedOrder = 2147483647

// ListPosition is where a page of the public list of assets starts: after
// the asset of this rank order and slug.
type ListPosition struct {
	Rank int    `json:"r"`
	Slug string `json:"s"`
}

// PositionOf is the position of an asset in the public list.
func PositionOf(a Asset) ListPosition {
	if a.MarketCapRank == nil {
		return ListPosition{Rank: unrankedOrder, Slug: a.Slug}
	}
	return ListPosition{Rank: *a.MarketCapRank, Slug: a.Slug}
}

// PublicAssets returns at most limit assets that are not hidden, by market
// cap rank, the unranked last, then by slug, after the position when it is
// set. A query matches the symbol exactly, or a part of the name or slug,
// in any case.
func (s *Store) PublicAssets(ctx context.Context, query string, after *ListPosition, limit int) ([]Asset, error) {
	query = strings.TrimSpace(query)
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
	afterRank, afterSlug := -1, ""
	if after != nil {
		afterRank, afterSlug = after.Rank, after.Slug
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+assetColumns+` FROM market_assets
		WHERE status <> 'hidden'
			AND ($1 = '' OR symbol = upper($1) OR name ILIKE $2 OR slug ILIKE $2)
			AND (COALESCE(market_cap_rank, $5), slug) > ($3, $4)
		ORDER BY COALESCE(market_cap_rank, $5), slug
		LIMIT $6`, query, like, afterRank, afterSlug, unrankedOrder, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PublicAsset returns the asset of a slug, and ErrNotFound for none or a
// hidden one.
func (s *Store) PublicAsset(ctx context.Context, slug string) (Asset, error) {
	a, err := scanAsset(s.db.QueryRowContext(ctx, `
		SELECT `+assetColumns+` FROM market_assets WHERE slug = $1 AND status <> 'hidden'`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	return a, err
}

// Contract is the contract of a token on a network.
type Contract struct {
	Network string `json:"network"`
	Address string `json:"address"`
}

// ContractsOf returns the contracts of an asset, by network.
func (s *Store) ContractsOf(ctx context.Context, assetID int64) ([]Contract, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT network, address FROM market_asset_contracts WHERE asset_id = $1 ORDER BY network, address`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Contract{}
	for rows.Next() {
		var c Contract
		if err := rows.Scan(&c.Network, &c.Address); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PairOf returns the id of the active pair of an asset at an enabled venue
// of the regions with the quote symbol, and ErrNotFound for none. No region
// is every region.
func (s *Store) PairOf(ctx context.Context, assetID int64, venueID, quote string, regions []string) (int64, error) {
	if regions == nil {
		regions = []string{}
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT p.id FROM market_venue_pairs p JOIN market_venues v ON v.id = p.venue_id
		WHERE p.base_asset_id = $1 AND p.venue_id = $2 AND p.quote_symbol = $3 AND p.active AND v.enabled
			AND (cardinality($4::text[]) = 0 OR v.region = ANY($4::text[]))
		ORDER BY p.id LIMIT 1`, assetID, venueID, NormalizeSymbol(quote), regions).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}
