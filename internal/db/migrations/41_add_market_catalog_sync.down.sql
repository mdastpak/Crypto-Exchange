DROP TABLE IF EXISTS market_identity_conflicts;
DROP INDEX IF EXISTS market_asset_sources_details;
ALTER TABLE market_asset_sources DROP COLUMN IF EXISTS details_fetched_at;
