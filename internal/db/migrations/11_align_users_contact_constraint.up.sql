-- Migration: 11_align_users_contact_constraint.up.sql
-- Description: Require the contact document written by OTP sign-in in
-- users.info, and allow at most one user per contact value.

-- Migration 2 required "username" and "password" keys, but OTP sign-in, the
-- only code that has ever created users, stores {"contact": {"<type>": "<value>"}},
-- so every first sign-in was rejected.
--
-- Preflight: rows without a contact object, or contact values shared by
-- several users, can only exist if they were inserted manually. Stop with an
-- actionable message instead of failing on a constraint or index. After fixing
-- the data, run `migrate force 10` and start the server again.
DO $$
DECLARE
    rows_without_contact BIGINT;
    duplicate_contacts BIGINT;
BEGIN
    SELECT COUNT(*) INTO rows_without_contact
    FROM users
    WHERE NOT COALESCE(jsonb_typeof(info->'contact') = 'object', FALSE);
    IF rows_without_contact > 0 THEN
        RAISE EXCEPTION 'migration 11: % user row(s) have no "contact" object in info; convert them to {"contact": {...}} or remove them, then retry', rows_without_contact;
    END IF;

    SELECT COUNT(*) INTO duplicate_contacts
    FROM (
        SELECT lower(info->'contact'->>'email') FROM users
        WHERE info->'contact' ? 'email' GROUP BY 1 HAVING COUNT(*) > 1
        UNION ALL
        SELECT info->'contact'->>'mobile' FROM users
        WHERE info->'contact' ? 'mobile' GROUP BY 1 HAVING COUNT(*) > 1
        UNION ALL
        SELECT lower(info->'contact'->>'username') FROM users
        WHERE info->'contact' ? 'username' GROUP BY 1 HAVING COUNT(*) > 1
    ) AS duplicates;
    IF duplicate_contacts > 0 THEN
        RAISE EXCEPTION 'migration 11: % contact value(s) are shared by more than one user (emails and usernames compared case-insensitively); merge or remove the duplicate users, then retry', duplicate_contacts;
    END IF;
END
$$;

ALTER TABLE users DROP CONSTRAINT IF EXISTS user_info_structure;

-- COALESCE turns the NULL produced by a missing key or a non-object info
-- (for example a JSON array) into a failed check instead of a passing one.
ALTER TABLE users ADD CONSTRAINT user_info_contact CHECK (
    COALESCE(jsonb_typeof(info->'contact') = 'object', FALSE)
);

-- One user per contact value. Email addresses and usernames are compared
-- case-insensitively. Rows without a given contact type index as NULL, which
-- never conflicts. Sign-in looks users up with these same expressions.
CREATE UNIQUE INDEX users_contact_email_unique ON users ((lower(info->'contact'->>'email')));
CREATE UNIQUE INDEX users_contact_mobile_unique ON users ((info->'contact'->>'mobile'));
CREATE UNIQUE INDEX users_contact_username_unique ON users ((lower(info->'contact'->>'username')));
