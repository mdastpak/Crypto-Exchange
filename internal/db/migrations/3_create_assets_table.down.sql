-- Migration: 3_create_assets_table.down.sql
-- Description: Drop assets table

DROP TRIGGER IF EXISTS update_assets_updated_at ON assets;
DROP TABLE IF EXISTS assets;
-- Created by this migration and used by the triggers of later ones, which
-- are rolled back before this one.
DROP FUNCTION IF EXISTS update_updated_at_column();
DROP TYPE IF EXISTS asset_type;
DROP TYPE IF EXISTS asset_status;
DROP TYPE IF EXISTS settlement_mode;