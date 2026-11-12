package marketdataadmin

import (
	"context"

	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/marketdata"
)

// Store is what the administration of the catalog reads and changes
// (#48): *marketdata.Store. The handler holds it as this interface, so that
// a test can give it a fake.
type Store interface {
	SourceControls(ctx context.Context) (map[string]marketdata.SourceControl, error)
	SourceUsages(ctx context.Context) (map[string]marketdata.SourceUsage, error)
	SetSourcePaused(ctx context.Context, source string, paused bool, reason string, origin audit.Event) (marketdata.SourceControl, error)
	JobStates(ctx context.Context) ([]marketdata.JobState, error)
	RunJobNow(ctx context.Context, job string, origin audit.Event) error
	ListRuns(ctx context.Context, f marketdata.RunFilter) ([]marketdata.Run, error)
	Venues(ctx context.Context) ([]marketdata.Venue, error)
	SetVenueEnabled(ctx context.Context, id string, enabled bool, reason string, origin audit.Event) (marketdata.Venue, error)
	AdminAssets(ctx context.Context, f marketdata.AssetFilter) ([]marketdata.AdminAsset, error)
	AdminAsset(ctx context.Context, id int64) (marketdata.AdminAsset, error)
	SetAssetHidden(ctx context.Context, id int64, hidden bool, reason string, origin audit.Event) (marketdata.AdminAsset, error)
	LinkAsset(ctx context.Context, id int64, currencyHK, reason string, origin audit.Event) (marketdata.AdminAsset, error)
	UnlinkAsset(ctx context.Context, id int64, reason string, origin audit.Event) (marketdata.AdminAsset, error)
	SetAssetTierOverride(ctx context.Context, assetID int64, tier *string, origin audit.Event) error
	AdminPairs(ctx context.Context, f marketdata.PairFilter) ([]marketdata.AdminPair, error)
	SetPairTierOverride(ctx context.Context, pairID int64, tier *string, origin audit.Event) error
	MapPair(ctx context.Context, pairID, baseID, quoteID int64, origin audit.Event) error
	ListConflicts(ctx context.Context, f marketdata.ConflictFilter) ([]marketdata.AdminConflict, error)
	ResolveConflict(ctx context.Context, id int64, note string, origin audit.Event) error
}

var _ Store = (*marketdata.Store)(nil)
