-- The public list of the newest assets (GET /market-data/assets?sort=newest)
-- reads the assets that are not hidden by first-collected time, newest
-- first, then by slug.
CREATE INDEX market_assets_newest ON market_assets (created_at DESC, slug) WHERE status <> 'hidden';
