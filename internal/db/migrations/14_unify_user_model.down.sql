-- Reverts migration 14: the contacts go back into users.info->'contact',
-- users.status back to the legacy enum, and the contact columns of
-- migration 8 come back empty.

-- users.info holds one contact of each type, so contacts that are not
-- primary cannot go back. Stop before changing anything.
DO $$
DECLARE
    secondary_contacts BIGINT;
BEGIN
    SELECT COUNT(*) INTO secondary_contacts FROM user_contacts WHERE NOT is_primary;
    IF secondary_contacts > 0 THEN
        RAISE EXCEPTION 'migration 14 down: % contact(s) are not primary, and users.info can only hold primary ones; remove them first', secondary_contacts;
    END IF;
END
$$;

-- The contact columns of migration 8, which were never written.
ALTER TABLE users ADD COLUMN email VARCHAR(255) UNIQUE;
ALTER TABLE users ADD COLUMN email_verified BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE users ADD COLUMN phone VARCHAR(50);
ALTER TABLE users ADD COLUMN phone_verified BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX idx_users_email ON users(email);

-- The JSONB document, as migration 11 left it.
ALTER TABLE users ADD COLUMN info JSONB;
UPDATE users u SET info = jsonb_build_object('contact', COALESCE(
    (SELECT jsonb_object_agg(c.type::text, c.value) FROM user_contacts c WHERE c.user_hk = u.hk),
    '{}'::jsonb
));
ALTER TABLE users ALTER COLUMN info SET NOT NULL;
ALTER TABLE users ADD CONSTRAINT user_info_contact CHECK (
    COALESCE(jsonb_typeof(info->'contact') = 'object', FALSE)
);
CREATE UNIQUE INDEX users_contact_email_unique ON users ((lower((info->'contact'->>'email') COLLATE "C")));
CREATE UNIQUE INDEX users_contact_mobile_unique ON users ((info->'contact'->>'mobile'));
CREATE UNIQUE INDEX users_contact_username_unique ON users ((lower((info->'contact'->>'username') COLLATE "C")));
-- From migration 2, which no later migration dropped.
CREATE UNIQUE INDEX user_info_username_unique ON users ((info->>'username'));

-- The legacy status. INACTIVE was disabled, or deleted when deleted_at is set.
CREATE TYPE user_status AS ENUM ('disabled', 'deleted', 'unverified', 'suspend', 'approved');
ALTER TABLE users ALTER COLUMN status DROP DEFAULT;
ALTER TABLE users ALTER COLUMN status DROP NOT NULL;
ALTER TABLE users ALTER COLUMN status TYPE user_status USING (
    CASE status::text
        WHEN 'ACTIVE' THEN 'approved'
        WHEN 'SUSPENDED' THEN 'suspend'
        WHEN 'BANNED' THEN 'disabled'
        WHEN 'INACTIVE' THEN CASE WHEN deleted_at IS NULL THEN 'disabled' ELSE 'deleted' END
        ELSE 'unverified'
    END
)::user_status;
ALTER TABLE users ALTER COLUMN status SET DEFAULT 'unverified';

DROP TABLE user_contacts;
DROP TYPE contact_type;
