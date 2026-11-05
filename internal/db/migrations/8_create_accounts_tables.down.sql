-- Migration: 8_create_accounts_tables.down.sql
-- Description: Drop accounts tables

-- Drop deferred foreign keys added in this migration's up.sql before
-- dropping sub_accounts (their referenced table).
ALTER TABLE orders DROP CONSTRAINT IF EXISTS fk_orders_sub_account;
ALTER TABLE balances DROP CONSTRAINT IF EXISTS fk_balances_sub_account;
ALTER TABLE balance_snapshots DROP CONSTRAINT IF EXISTS fk_balance_snapshots_sub_account;
ALTER TABLE balance_changes DROP CONSTRAINT IF EXISTS fk_balance_changes_sub_account;

DROP TRIGGER IF EXISTS update_sub_accounts_updated_at ON sub_accounts;
DROP TRIGGER IF EXISTS update_api_keys_updated_at ON api_keys;

DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS sub_accounts;

ALTER TABLE users DROP COLUMN IF EXISTS email;
ALTER TABLE users DROP COLUMN IF EXISTS email_verified;
ALTER TABLE users DROP COLUMN IF EXISTS phone;
ALTER TABLE users DROP COLUMN IF EXISTS phone_verified;
ALTER TABLE users DROP COLUMN IF EXISTS totp_secret;
ALTER TABLE users DROP COLUMN IF EXISTS totp_enabled;
ALTER TABLE users DROP COLUMN IF EXISTS first_name;
ALTER TABLE users DROP COLUMN IF EXISTS last_name;
ALTER TABLE users DROP COLUMN IF EXISTS display_name;
ALTER TABLE users DROP COLUMN IF EXISTS avatar_url;
-- Migration 2 created users.status with the USER_STATUS type, so the up
-- migration's ADD COLUMN IF NOT EXISTS status never ran, and dropping the
-- column here deleted the status of every user. Drop it only if this
-- migration created it, which is when it has this migration's type. The
-- index on it was created by this migration either way.
DROP INDEX IF EXISTS idx_users_status;
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = current_schema() AND table_name = 'users'
          AND column_name = 'status' AND udt_name = 'account_status_type'
    ) THEN
        ALTER TABLE users DROP COLUMN status;
    END IF;
END
$$;
ALTER TABLE users DROP COLUMN IF EXISTS last_login_at;
ALTER TABLE users DROP COLUMN IF EXISTS last_login_ip;
ALTER TABLE users DROP COLUMN IF EXISTS referral_code;
ALTER TABLE users DROP COLUMN IF EXISTS referred_by;
ALTER TABLE users DROP COLUMN IF EXISTS kyc_status;
ALTER TABLE users DROP COLUMN IF EXISTS kyc_tier;
ALTER TABLE users DROP COLUMN IF EXISTS risk_score;

DROP TYPE IF EXISTS account_status_type;
DROP TYPE IF EXISTS kyc_status;
DROP TYPE IF EXISTS kyc_tier;
DROP TYPE IF EXISTS sub_account_status;
DROP TYPE IF EXISTS api_key_type;
DROP TYPE IF EXISTS api_key_status;
DROP TYPE IF EXISTS auth_method;
DROP TYPE IF EXISTS session_status;