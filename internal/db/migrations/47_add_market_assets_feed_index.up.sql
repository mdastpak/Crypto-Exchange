-- The feed of the market data catalog for the blog service (#271) reads the
-- assets in the order they changed, after a position (updated_at, slug).
CREATE INDEX market_assets_updated ON market_assets (updated_at, slug);
