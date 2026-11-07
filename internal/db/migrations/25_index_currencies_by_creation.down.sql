DROP INDEX IF EXISTS idx_currencies_created_hk;
ALTER TABLE currencies
    ALTER COLUMN created_at DROP NOT NULL,
    ALTER COLUMN updated_at DROP NOT NULL;
