ALTER TABLE user_contacts
    DROP COLUMN ownership_checked_at,
    DROP COLUMN ownership_provider,
    DROP COLUMN ownership_ref;

DROP TABLE user_national_ids;
