-- The admin list of currencies is sorted by creation, newest first, with the
-- hk to break ties, and pages by keyset on both (#198). The sort key cannot
-- be NULL, or rows would fall out of the keyset comparison.
UPDATE currencies SET created_at = NOW() WHERE created_at IS NULL;
UPDATE currencies SET updated_at = created_at WHERE updated_at IS NULL;
ALTER TABLE currencies
    ALTER COLUMN created_at SET NOT NULL,
    ALTER COLUMN updated_at SET NOT NULL;
CREATE INDEX idx_currencies_created_hk ON currencies (created_at DESC, hk DESC);
