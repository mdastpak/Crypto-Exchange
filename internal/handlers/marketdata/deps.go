package marketdata

import (
	"context"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/marketdata"
)

// Store is what the public market data routes read of the catalog (#48):
// *marketdata.Store. The handler holds it as this interface, so that a test
// can give it a fake.
type Store interface {
	PublicAssets(ctx context.Context, query string, after *marketdata.ListPosition, limit int) ([]marketdata.Asset, error)
	NewestAssets(ctx context.Context, query string, after *marketdata.NewestPosition, limit int) ([]marketdata.Asset, error)
	PublicAsset(ctx context.Context, slug string) (marketdata.Asset, error)
	FeedAssets(ctx context.Context, after marketdata.FeedPosition, delay time.Duration, limit int) ([]marketdata.Asset, error)
	ContractsOf(ctx context.Context, assetID int64) ([]marketdata.Contract, error)
	ContractsOfAssets(ctx context.Context, ids []int64) (map[int64][]marketdata.Contract, error)
	QuotesOf(ctx context.Context, assetID int64, regions []string) ([]marketdata.Quote, error)
	PairOf(ctx context.Context, assetID int64, venueID, quote string, regions []string) (int64, error)
	Candles(ctx context.Context, pairID int64, resolution string, from, to time.Time) ([]marketdata.Candle, error)
}

var _ Store = (*marketdata.Store)(nil)
