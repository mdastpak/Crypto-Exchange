DROP TABLE IF EXISTS currency_delisting_steps;
DROP TABLE IF EXISTS currency_delistings;
DROP TABLE IF EXISTS currency_capability_overrides;
ALTER TABLE currencies DROP COLUMN IF EXISTS lifecycle_changed_at, DROP COLUMN IF EXISTS lifecycle;
