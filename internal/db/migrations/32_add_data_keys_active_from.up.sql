-- Two-phase rotation of the data keys (#233). A new version is written to
-- data_keys at once, so that every server instance loads it and can read
-- data protected with it, but instances start writing with it only from
-- active_from, which keys rotate sets a delay into the future. The delay
-- is longer than the reload interval of the instances, so no instance
-- writes with a version that another instance has not loaded yet.
ALTER TABLE data_keys ADD COLUMN active_from TIMESTAMPTZ;
UPDATE data_keys SET active_from = created_at;
ALTER TABLE data_keys ALTER COLUMN active_from SET NOT NULL;
ALTER TABLE data_keys ALTER COLUMN active_from SET DEFAULT NOW();
