package marketdata

import (
	"context"
	"errors"
	"time"
)

// FeedPosition is where a page of the feed of changed assets starts: after
// the asset that changed at UpdatedAt with this slug. The slug breaks ties
// with a public name, never with the internal id.
type FeedPosition struct {
	UpdatedAt time.Time `json:"u"`
	Slug      string    `json:"s"`
}

// FeedPositionOf is the position of an asset in the feed.
func FeedPositionOf(a Asset) FeedPosition {
	return FeedPosition{UpdatedAt: a.UpdatedAt.UTC(), Slug: a.Slug}
}

// FeedAssets returns at most limit assets, hidden ones too, in the order
// they last changed, after the position, that changed at least delay ago
// by the database clock.
//
// The delay lets the writes in progress commit: updated_at is the start of
// the writing transaction, so a transaction that commits late could
// otherwise add a row before a position that a reader has already passed.
// Every write of market_assets is one short statement or transaction, far
// shorter than the delay.
func (s *Store) FeedAssets(ctx context.Context, after FeedPosition, delay time.Duration, limit int) ([]Asset, error) {
	if delay < 0 {
		return nil, errors.New("marketdata: a negative feed delay")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+assetColumns+` FROM market_assets
		WHERE (updated_at, slug) > ($1, $2) AND updated_at <= NOW() - make_interval(secs => $3)
		ORDER BY updated_at, slug
		LIMIT $4`, after.UpdatedAt, after.Slug, delay.Seconds(), limit)
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

// ContractsOfAssets returns the contracts of the assets, by asset id and
// then by network, in one query.
func (s *Store) ContractsOfAssets(ctx context.Context, ids []int64) (map[int64][]Contract, error) {
	out := map[int64][]Contract{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT asset_id, network, address FROM market_asset_contracts
		WHERE asset_id = ANY($1::bigint[]) ORDER BY asset_id, network, address`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var c Contract
		if err := rows.Scan(&id, &c.Network, &c.Address); err != nil {
			return nil, err
		}
		out[id] = append(out[id], c)
	}
	return out, rows.Err()
}
