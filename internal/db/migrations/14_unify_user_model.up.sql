-- Migration 14 settles the user model (#38). The contacts move from
-- users.info->'contact' into user_contacts, users.status becomes
-- account_status_type, and info and the contact columns of migration 8,
-- which were never written, are dropped.

-- Preflight: stop before changing anything if a row holds data that this
-- migration has no place for. The application only writes
-- {"contact": {<type>: <value>}} with an email, mobile, or username string,
-- and never writes the contact columns of migration 8.
DO $$
DECLARE
    other_info BIGINT;
    other_contacts BIGINT;
    filled_columns BIGINT;
BEGIN
    SELECT COUNT(*) INTO other_info
    FROM users
    WHERE EXISTS (SELECT 1 FROM jsonb_object_keys(info) AS k WHERE k <> 'contact');
    IF other_info > 0 THEN
        RAISE EXCEPTION 'migration 14: % user row(s) have keys other than "contact" in info, which this migration would drop; export those keys and remove them, then run "migrate force 13" and start the server again', other_info;
    END IF;

    SELECT COUNT(*) INTO other_contacts
    FROM users, jsonb_each(info->'contact') AS c(k, v)
    WHERE c.k NOT IN ('email', 'mobile', 'username') OR jsonb_typeof(c.v) NOT IN ('string', 'null');
    IF other_contacts > 0 THEN
        RAISE EXCEPTION 'migration 14: % contact value(s) are not an email, mobile, or username string; fix or remove them, then run "migrate force 13" and start the server again', other_contacts;
    END IF;

    SELECT COUNT(*) INTO filled_columns
    FROM users
    WHERE email IS NOT NULL OR phone IS NOT NULL OR email_verified OR phone_verified;
    IF filled_columns > 0 THEN
        RAISE EXCEPTION 'migration 14: % user row(s) have values in the email or phone columns, which this migration drops; move them into info->''contact'' or clear them, then run "migrate force 13" and start the server again', filled_columns;
    END IF;
END
$$;

-- Contacts. A user's first contact proves itself at the first sign-in, since
-- a users row is only created after a correct code, so email and mobile
-- contacts count as verified at the row's creation. Usernames cannot sign in
-- (#14) and stay unverified.
CREATE TYPE contact_type AS ENUM ('email', 'mobile', 'username');

CREATE TABLE user_contacts (
    id          BIGSERIAL PRIMARY KEY,
    user_hk     UUID NOT NULL REFERENCES users(hk) ON DELETE CASCADE,
    type        contact_type NOT NULL,
    -- Emails keep the case they were entered with; mobiles are E.164.
    value       TEXT NOT NULL CHECK (value <> ''),
    verified_at TIMESTAMPTZ,
    is_primary  BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One owner per contact value. Emails and usernames compare
-- case-insensitively, with ASCII-only case folding that does not depend on
-- the database locale, as in migration 11; lower() leaves mobiles unchanged.
CREATE UNIQUE INDEX user_contacts_value_unique ON user_contacts (type, lower(value COLLATE "C"));
-- At most one primary contact of each type per user.
CREATE UNIQUE INDEX user_contacts_primary_unique ON user_contacts (user_hk, type) WHERE is_primary;
CREATE INDEX user_contacts_user_hk ON user_contacts (user_hk);

INSERT INTO user_contacts (user_hk, type, value, verified_at, created_at, updated_at)
SELECT u.hk,
       c.k::contact_type,
       c.v #>> '{}',
       CASE WHEN c.k IN ('email', 'mobile') THEN COALESCE(u.created_at, NOW()) END,
       COALESCE(u.created_at, NOW()),
       COALESCE(u.updated_at, u.created_at, NOW())
FROM users u, jsonb_each(u.info->'contact') AS c(k, v)
WHERE jsonb_typeof(c.v) = 'string';

-- Status. deleted_at becomes the only mark of deletion, so a deleted user
-- gets one if it had none. A missing status meant the column default,
-- 'unverified'.
UPDATE users SET deleted_at = COALESCE(updated_at, NOW()) WHERE status = 'deleted' AND deleted_at IS NULL;
ALTER TABLE users ALTER COLUMN status DROP DEFAULT;
ALTER TABLE users ALTER COLUMN status TYPE account_status_type USING (
    CASE status::text
        WHEN 'approved' THEN 'ACTIVE'
        WHEN 'suspend' THEN 'SUSPENDED'
        WHEN 'disabled' THEN 'INACTIVE'
        WHEN 'deleted' THEN 'INACTIVE'
        ELSE 'PENDING_VERIFICATION'
    END
)::account_status_type;
ALTER TABLE users ALTER COLUMN status SET DEFAULT 'PENDING_VERIFICATION';
ALTER TABLE users ALTER COLUMN status SET NOT NULL;
DROP TYPE user_status;

-- The JSONB document and the unused contact columns.
DROP INDEX users_contact_email_unique;
DROP INDEX users_contact_mobile_unique;
DROP INDEX users_contact_username_unique;
ALTER TABLE users DROP CONSTRAINT user_info_contact;
-- Dropping info also drops user_info_username_unique of migration 2.
ALTER TABLE users DROP COLUMN info;
-- Dropping email also drops its unique constraint and idx_users_email.
ALTER TABLE users DROP COLUMN email;
ALTER TABLE users DROP COLUMN email_verified;
ALTER TABLE users DROP COLUMN phone;
ALTER TABLE users DROP COLUMN phone_verified;
