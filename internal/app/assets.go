package app

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mdastpak/crypto-exchange/internal/assetregistry"
	"github.com/mdastpak/crypto-exchange/internal/audit"
	"github.com/mdastpak/crypto-exchange/internal/config"
	"github.com/mdastpak/crypto-exchange/internal/currencies"
)

// assetSyncTimeout bounds the import at startup, which waits for an import of
// another instance under the same lock.
const assetSyncTimeout = time.Minute

// syncAssets adds at startup what the asset registry has and the database
// lacks (#57, assets.auto_sync): currencies, networks, and markets that do
// not exist. It never changes one that exists, so that a restart does not
// undo what administrators changed. The registry is assets.registry_path,
// or the one built into this release. Every instance runs it; a lock makes
// them run one after the other, and the later ones find nothing to add. A
// registry that cannot be read or fails its checks keeps the server from
// starting: it is part of the release. It reports whether anything was
// added, which the public list then shows.
func syncAssets(ctx context.Context, db *sql.DB, settings *config.Configuration) (bool, error) {
	if !settings.Assets.AutoSync {
		return false, nil
	}
	reg, source, err := assetregistry.LoadOrBuiltIn(settings.Assets.RegistryPath)
	if err != nil {
		return false, fmt.Errorf("assets.auto_sync: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, assetSyncTimeout)
	defer cancel()
	host, _ := os.Hostname()
	report, err := currencies.Store{DB: db}.Sync(ctx, reg, audit.Operator("server assets.auto_sync", "", "", host))
	if err != nil {
		return false, fmt.Errorf("assets.auto_sync of %s: %w", source, err)
	}
	log.Printf("Synced the asset registry from %s (assets.auto_sync): currencies created: %d (%s), networks created: %d, markets created: %d; the rest exist and were left as they are",
		source, len(report.Created), strings.Join(report.Created, ", "), report.Networks.Created, report.Markets.Created)
	for _, note := range report.Notes {
		log.Printf("Asset registry: %s", note)
	}
	return len(report.Created) > 0 || report.Networks.Created > 0 || report.Markets.Created > 0, nil
}
