package marketdata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// The collection tiers of the pairs (#279). The tier of a pair decides at
// which candle resolutions it is collected.
const (
	TierHot    = "hot"
	TierActive = "active"
	TierOther  = "other"
)

// Tiers lists the tiers, the most watched first.
var Tiers = []string{TierHot, TierActive, TierOther}

// effectiveTier is the tier of a pair p whose base asset is a: an
// administrator's tier on the pair, else on the asset, else the automatic
// one.
const effectiveTier = `COALESCE(p.tier_override, a.tier_override, p.tier)`

// TierPair is a pair with what the tiers job needs: its base, its quote,
// its latest ticker, and its tiers.
type TierPair struct {
	PairID      int64
	VenueID     string
	BaseSymbol  string
	QuoteSymbol string
	BaseSlug    string
	// Listed: the base is linked to a currency listed on this exchange.
	Listed bool
	// Tier is the automatic tier; Override the administrator's, on the
	// pair or its base, if any.
	Tier     string
	Override *string
	// The latest ticker; Last is nil without one.
	Last        *shared.Decimal
	VolumeBase  *shared.Decimal
	VolumeQuote *shared.Decimal
}

// TierPairs returns the active pairs of the enabled venues whose base is a
// catalog asset that is not hidden.
func (s *Store) TierPairs(ctx context.Context) ([]TierPair, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.venue_id, p.base_symbol, p.quote_symbol, a.slug, a.status = 'linked',
			p.tier, COALESCE(p.tier_override, a.tier_override), t.last_price, t.volume_base, t.volume_quote
		FROM market_venue_pairs p
		JOIN market_venues v ON v.id = p.venue_id
		JOIN market_assets a ON a.id = p.base_asset_id
		LEFT JOIN market_tickers_latest t ON t.pair_id = p.id
		WHERE p.active AND v.enabled AND a.status <> 'hidden'
		ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TierPair
	for rows.Next() {
		var p TierPair
		if err := rows.Scan(&p.PairID, &p.VenueID, &p.BaseSymbol, &p.QuoteSymbol, &p.BaseSlug, &p.Listed,
			&p.Tier, &p.Override, &p.Last, &p.VolumeBase, &p.VolumeQuote); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetTiers writes the automatic tiers of pairs, by pair id, in one
// statement, and returns how many changed. A pair already in its tier is
// not written.
func (s *Store) SetTiers(ctx context.Context, tiers map[int64]string) (int, error) {
	if len(tiers) == 0 {
		return 0, nil
	}
	ids := make([]int64, 0, len(tiers))
	names := make([]string, 0, len(tiers))
	for id, tier := range tiers {
		if !slices.Contains(Tiers, tier) {
			return 0, fmt.Errorf("marketdata: unknown tier %q", tier)
		}
		ids = append(ids, id)
		names = append(names, tier)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE market_venue_pairs p SET tier = n.tier, tier_changed_at = NOW()
		FROM unnest($1::bigint[], $2::text[]) AS n(id, tier)
		WHERE p.id = n.id AND p.tier <> n.tier`, ids, names)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// SetPairTierOverride sets the administrator's tier of a pair, or clears it
// with nil (#270).
func (s *Store) SetPairTierOverride(ctx context.Context, pairID int64, tier *string) error {
	return s.setTierOverride(ctx, "market_venue_pairs", pairID, tier)
}

// SetAssetTierOverride sets the administrator's tier of the pairs of an
// asset, or clears it with nil (#270). A tier on a pair wins over it.
func (s *Store) SetAssetTierOverride(ctx context.Context, assetID int64, tier *string) error {
	return s.setTierOverride(ctx, "market_assets", assetID, tier)
}

func (s *Store) setTierOverride(ctx context.Context, table string, id int64, tier *string) error {
	if tier != nil && !slices.Contains(Tiers, *tier) {
		return fmt.Errorf("marketdata: unknown tier %q", *tier)
	}
	// table is one of two constants.
	res, err := s.db.ExecContext(ctx, `UPDATE `+table+` SET tier_override = $2 WHERE id = $1`, id, tier) //nolint:gosec // G202: a constant table name
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PairTier returns the tier in which a pair is collected.
func (s *Store) PairTier(ctx context.Context, pairID int64) (string, error) {
	var tier string
	err := s.db.QueryRowContext(ctx, `
		SELECT `+effectiveTier+` FROM market_venue_pairs p
		LEFT JOIN market_assets a ON a.id = p.base_asset_id WHERE p.id = $1`, pairID).Scan(&tier)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return tier, err
}
