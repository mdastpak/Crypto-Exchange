package marketdata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// Listing are the fields of an asset that a source's listing gives in bulk.
// The catalog sync writes them with UpdateListing, which keeps the
// description, links, and kind that only the full record gives.
type Listing struct {
	Symbol            string
	Name              string
	LogoURL           string
	MarketCapRank     *int
	CirculatingSupply *shared.Decimal
	TotalSupply       *shared.Decimal
	MaxSupply         *shared.Decimal
	Attribution       string
}

// Conflict kinds.
const (
	// ConflictContract: a source gives a contract to an asset other than
	// the catalog's owner of the contract.
	ConflictContract = "contract"
	// ConflictIdentity: a source's id is linked to one asset, and its
	// contracts point at another.
	ConflictIdentity = "identity"
	// ConflictSymbol: a secondary source lists an asset whose symbol the
	// catalog has, without proof that they are the same asset.
	ConflictSymbol = "symbol"
)

// Conflict is what the catalog sync could not decide alone; an
// administrator resolves it (#270).
type Conflict struct {
	Source     string
	ExternalID string
	Kind       string
	Subject    string
	AssetID    *int64
	ClaimedBy  *int64
	Detail     string
}

// UpdateListing writes the listing fields of an asset and marks it active,
// only when a value differs, and reports whether it changed the row. A
// figure that the listing lacks (nil) keeps the stored one, so that a
// listing never erases what the full record gave.
func (s *Store) UpdateListing(ctx context.Context, id int64, l Listing) (bool, error) {
	l.Symbol, l.Name = NormalizeSymbol(l.Symbol), strings.TrimSpace(l.Name)
	if l.Symbol == "" || utf8.RuneCountInString(l.Symbol) > 32 || l.Name == "" || utf8.RuneCountInString(l.Name) > 200 {
		return false, fmt.Errorf("marketdata: invalid listing %q %q", l.Symbol, l.Name)
	}
	if l.LogoURL != "" && !strings.HasPrefix(l.LogoURL, "https://") {
		l.LogoURL = ""
	}
	if l.MarketCapRank != nil && *l.MarketCapRank <= 0 {
		l.MarketCapRank = nil
	}
	for _, supply := range []**shared.Decimal{&l.CirculatingSupply, &l.TotalSupply, &l.MaxSupply} {
		if *supply != nil && (*supply).IsNegative() {
			*supply = nil
		}
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE market_assets SET symbol = $2, name = $3,
			logo_url = CASE WHEN $4 = '' THEN logo_url ELSE $4 END,
			market_cap_rank = $5, circulating_supply = COALESCE($6, circulating_supply),
			total_supply = COALESCE($7, total_supply), max_supply = COALESCE($8, max_supply),
			attribution = $9, active = TRUE, updated_at = NOW()
		WHERE id = $1 AND (symbol, name, logo_url, market_cap_rank, circulating_supply, total_supply,
			max_supply, attribution, active)
			IS DISTINCT FROM ($2, $3, CASE WHEN $4 = '' THEN logo_url ELSE $4 END, $5::integer,
			COALESCE($6::numeric, circulating_supply), COALESCE($7::numeric, total_supply),
			COALESCE($8::numeric, max_supply), $9, TRUE)`,
		id, l.Symbol, l.Name, l.LogoURL, l.MarketCapRank, l.CirculatingSupply, l.TotalSupply, l.MaxSupply, l.Attribution)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SourcesOf returns the sources that know an asset.
func (s *Store) SourcesOf(ctx context.Context, assetID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT source FROM market_asset_sources WHERE asset_id = $1 ORDER BY source`, assetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sources []string
	for rows.Next() {
		var src string
		if err := rows.Scan(&src); err != nil {
			return nil, err
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

// AssetsBySymbol returns the ids of the assets with the symbol, and of
// those the ids whose name gives the slug nameSlug.
func (s *Store) AssetsBySymbol(ctx context.Context, symbol, nameSlug string) (all, sameName []int64, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM market_assets WHERE symbol = $1 ORDER BY id`, NormalizeSymbol(symbol))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, nil, err
		}
		all = append(all, id)
		if nameSlug != "" && Slugify(name) == nameSlug {
			sameName = append(sameName, id)
		}
	}
	return all, sameName, rows.Err()
}

// DetailsDue returns at most limit identities of the source whose full
// record was never read, or read before olderThan: the never-read first,
// then the oldest, the better-ranked first among equals.
func (s *Store) DetailsDue(ctx context.Context, source string, olderThan time.Time, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.external_id FROM market_asset_sources s JOIN market_assets a ON a.id = s.asset_id
		WHERE s.source = $1 AND a.status <> 'hidden'
			AND (s.details_fetched_at IS NULL OR s.details_fetched_at < $2)
		ORDER BY s.details_fetched_at NULLS FIRST, a.market_cap_rank NULLS LAST, s.external_id
		LIMIT $3`, source, olderThan, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MarkDetailsFetched records that the full record of an identity was read
// at at.
func (s *Store) MarkDetailsFetched(ctx context.Context, source, externalID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE market_asset_sources SET details_fetched_at = $3
		WHERE source = $1 AND external_id = $2`, source, externalID, at)
	return err
}

// RecordConflict records a conflict for an administrator, once while it is
// open, and reports whether it is new.
func (s *Store) RecordConflict(ctx context.Context, c Conflict) (bool, error) {
	switch c.Kind {
	case ConflictContract, ConflictIdentity, ConflictSymbol:
	default:
		return false, fmt.Errorf("marketdata: invalid conflict kind %q", c.Kind)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO market_identity_conflicts (source, external_id, kind, subject, asset_id, claimed_by, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (source, external_id, kind, subject) WHERE resolved_at IS NULL DO NOTHING`,
		c.Source, c.ExternalID, c.Kind, c.Subject, c.AssetID, c.ClaimedBy, truncate(c.Detail, 1000))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// OpenConflicts returns the open conflicts of a source, oldest first.
func (s *Store) OpenConflicts(ctx context.Context, source string) ([]Conflict, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source, external_id, kind, subject, asset_id, claimed_by, detail
		FROM market_identity_conflicts WHERE source = $1 AND resolved_at IS NULL ORDER BY id`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conflict
	for rows.Next() {
		var c Conflict
		if err := rows.Scan(&c.Source, &c.ExternalID, &c.Kind, &c.Subject, &c.AssetID, &c.ClaimedBy, &c.Detail); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeactivateUnseen marks inactive the active assets that no source listed
// since before, and returns how many. An inactive asset stays in the
// catalog; a later listing makes it active again.
func (s *Store) DeactivateUnseen(ctx context.Context, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, errors.New("marketdata: no time to deactivate before")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE market_assets SET active = FALSE, updated_at = NOW()
		WHERE active AND last_seen_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Record are the fields of an asset that only its full record at a source
// gives: kind, description, links, and logo.
type Record struct {
	Kind        string
	Description string
	Links       map[string]string
	LogoURL     string
}

// UpdateRecord writes the record fields of an asset, only when a value
// differs, and reports whether it changed the row. An empty logo keeps the
// stored one; the listing fields are left as they are.
func (s *Store) UpdateRecord(ctx context.Context, id int64, r Record) (bool, error) {
	switch r.Kind {
	case "":
		r.Kind = KindUnknown
	case KindCoin, KindToken, KindUnknown:
	default:
		return false, fmt.Errorf("marketdata: invalid kind %q", r.Kind)
	}
	links := map[string]string{}
	for kind, link := range r.Links {
		if strings.HasPrefix(link, "https://") {
			links[kind] = link
		}
	}
	if r.LogoURL != "" && !strings.HasPrefix(r.LogoURL, "https://") {
		r.LogoURL = ""
	}
	encoded, err := json.Marshal(links)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE market_assets SET kind = $2, description = $3, links = $4,
			logo_url = CASE WHEN $5 = '' THEN logo_url ELSE $5 END, updated_at = NOW()
		WHERE id = $1 AND (kind, description, links, logo_url)
			IS DISTINCT FROM ($2, $3, $4::jsonb, CASE WHEN $5 = '' THEN logo_url ELSE $5 END)`,
		id, r.Kind, truncate(r.Description, 20000), encoded, r.LogoURL)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
