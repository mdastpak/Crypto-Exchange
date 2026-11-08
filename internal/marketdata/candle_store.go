package marketdata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/domain/shared"
)

// The resolutions of candles.
const (
	Resolution5m = "5m"
	Resolution1h = "1h"
	Resolution1d = "1d"
)

// Resolutions lists the resolutions of candles.
var Resolutions = []string{Resolution5m, Resolution1h, Resolution1d}

// ResolutionStep returns the length of a candle of the resolution, and 0
// for an unknown one.
func ResolutionStep(resolution string) time.Duration {
	switch resolution {
	case Resolution5m:
		return 5 * time.Minute
	case Resolution1h:
		return time.Hour
	case Resolution1d:
		return 24 * time.Hour
	}
	return 0
}

// Candle is one OHLCV candle of a pair, which opens at OpenTime, in UTC.
// Volume is in the base asset.
type Candle struct {
	OpenTime               time.Time
	Open, High, Low, Close shared.Decimal
	Volume                 shared.Decimal
}

// CandlePair is a pair whose candles a job keeps, with what it has asked
// the venue for: the candles in [From, Until) are stored, or the venue had
// none. Both are nil before the first request.
type CandlePair struct {
	PairID      int64
	VenueSymbol string
	From, Until *time.Time
}

// CandlePairs returns at most limit active pairs of an enabled venue whose
// base is mapped to the catalog and whose quote is one of quotes, with
// their cursors at the resolution: the pairs never asked first, then those
// whose candles are the oldest, the better-ranked base first.
func (s *Store) CandlePairs(ctx context.Context, venueID, resolution string, quotes []string, limit int) ([]CandlePair, error) {
	normalized := make([]string, len(quotes))
	for i, q := range quotes {
		normalized[i] = NormalizeSymbol(q)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.venue_symbol, c.from_time, c.until_time
		FROM market_venue_pairs p
		JOIN market_venues v ON v.id = p.venue_id
		JOIN market_assets a ON a.id = p.base_asset_id
		LEFT JOIN market_candle_cursors c ON c.pair_id = p.id AND c.resolution = $2
		WHERE p.venue_id = $1 AND p.active AND v.enabled AND a.status <> 'hidden'
			AND p.quote_symbol = ANY($3::text[])
		ORDER BY a.market_cap_rank NULLS LAST, p.id
		LIMIT $4`, venueID, resolution, normalized, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CandlePair
	for rows.Next() {
		var p CandlePair
		if err := rows.Scan(&p.PairID, &p.VenueSymbol, &p.From, &p.Until); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// WriteCandles stores the candles of a pair that the venue gave for the
// range [from, until), and widens the pair's cursor to cover the range, in
// one transaction. A candle already stored is replaced, so that a repeated
// request does no harm. The range must touch the cursor, as the next
// forward or backward range does. It creates the partitions it needs.
func (s *Store) WriteCandles(ctx context.Context, pairID int64, resolution string, from, until time.Time, candles []Candle) (int, error) {
	step := ResolutionStep(resolution)
	if step == 0 {
		return 0, fmt.Errorf("marketdata: unknown resolution %q", resolution)
	}
	if !from.Before(until) {
		return 0, errors.New("marketdata: an empty range of candles")
	}
	n := len(candles)
	opens := make([]time.Time, n)
	o, h, l, c, v := make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	periods := map[time.Time]bool{}
	for i, cd := range candles {
		t := cd.OpenTime.UTC()
		if t.Before(from) || !t.Before(until) || !t.Equal(t.Truncate(step)) {
			return 0, fmt.Errorf("marketdata: candle at %s outside [%s, %s) or not aligned", t, from, until)
		}
		if cd.Low.Decimal().GreaterThan(cd.High.Decimal()) || cd.Open.IsNegative() || cd.Low.IsNegative() || cd.Close.IsNegative() || cd.Volume.IsNegative() {
			return 0, fmt.Errorf("marketdata: invalid candle at %s", t)
		}
		opens[i] = t
		o[i], h[i], l[i], c[i], v[i] = cd.Open.String(), cd.High.String(), cd.Low.String(), cd.Close.String(), cd.Volume.String()
		periods[partitionStart(resolution, t)] = true
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	for start := range periods {
		if _, err := tx.ExecContext(ctx, `SELECT market_candles_ensure_partition($1, $2)`, resolution, start); err != nil {
			return 0, fmt.Errorf("marketdata: candle partition: %w", err)
		}
	}
	written := 0
	if n > 0 {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO market_candles (pair_id, resolution, open_time, open, high, low, close, volume)
			SELECT $1, $2, t.* FROM unnest($3::timestamptz[], $4::numeric[], $5::numeric[], $6::numeric[],
				$7::numeric[], $8::numeric[]) AS t
			ON CONFLICT (pair_id, resolution, open_time) DO UPDATE SET open = excluded.open, high = excluded.high,
				low = excluded.low, close = excluded.close, volume = excluded.volume, fetched_at = NOW()
			WHERE (market_candles.open, market_candles.high, market_candles.low, market_candles.close, market_candles.volume)
				IS DISTINCT FROM (excluded.open, excluded.high, excluded.low, excluded.close, excluded.volume)`,
			pairID, resolution, opens, o, h, l, c, v)
		if err != nil {
			return 0, err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		written = int(rows)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO market_candle_cursors (pair_id, resolution, from_time, until_time)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (pair_id, resolution) DO UPDATE SET
			from_time = LEAST(market_candle_cursors.from_time, excluded.from_time),
			until_time = GREATEST(market_candle_cursors.until_time, excluded.until_time),
			updated_at = NOW()`, pairID, resolution, from.UTC(), until.UTC()); err != nil {
		return 0, err
	}
	return written, tx.Commit()
}

// partitionStart is the start of the partition of a resolution that holds
// t: its year for 1d, its month otherwise.
func partitionStart(resolution string, t time.Time) time.Time {
	t = t.UTC()
	if resolution == Resolution1d {
		return time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// Candles returns the stored candles of a pair that open in [from, to),
// oldest first.
func (s *Store) Candles(ctx context.Context, pairID int64, resolution string, from, to time.Time) ([]Candle, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT open_time, open, high, low, close, volume FROM market_candles
		WHERE pair_id = $1 AND resolution = $2 AND open_time >= $3 AND open_time < $4
		ORDER BY open_time`, pairID, resolution, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Candle
	for rows.Next() {
		var c Candle
		if err := rows.Scan(&c.OpenTime, &c.Open, &c.High, &c.Low, &c.Close, &c.Volume); err != nil {
			return nil, err
		}
		c.OpenTime = c.OpenTime.UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// EnsureCandlePartitions creates the partitions of a resolution for the
// current period and the next, so that writes never wait for one.
func (s *Store) EnsureCandlePartitions(ctx context.Context, resolution string, now time.Time) error {
	if ResolutionStep(resolution) == 0 {
		return fmt.Errorf("marketdata: unknown resolution %q", resolution)
	}
	current := partitionStart(resolution, now)
	next := current.AddDate(0, 1, 0)
	if resolution == Resolution1d {
		next = current.AddDate(1, 0, 0)
	}
	for _, at := range []time.Time{current, next} {
		if _, err := s.db.ExecContext(ctx, `SELECT market_candles_ensure_partition($1, $2)`, resolution, at); err != nil {
			return fmt.Errorf("marketdata: candle partition: %w", err)
		}
	}
	return nil
}

// DropCandlePartitions drops the partitions of a resolution whose candles
// all open before before, and returns how many.
func (s *Store) DropCandlePartitions(ctx context.Context, resolution string, before time.Time) (int, error) {
	var dropped int
	err := s.db.QueryRowContext(ctx, `SELECT market_candles_drop_partitions($1, $2)`, resolution, before).Scan(&dropped)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return dropped, err
}

// CandlePartitions returns the names of the partitions of a resolution,
// oldest first.
func (s *Store) CandlePartitions(ctx context.Context, resolution string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'market_candles_' || $1 ORDER BY c.relname`, resolution)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
