-- Migration: 11_align_users_contact_constraint.up.sql
-- Description: Align the users.info constraint with the document written by
-- OTP sign-in, and allow at most one user per contact value.

-- Migration 2 required "username" and "password" keys, but OTP sign-in stores
-- {"contact": {"<type>": "<value>"}}, so every first sign-in was rejected.
ALTER TABLE users DROP CONSTRAINT IF EXISTS user_info_structure;

-- Rows written by the earlier username and password flow stay valid until the
-- user model is unified; every other row must carry a contact object.
ALTER TABLE users ADD CONSTRAINT user_info_contact CHECK (
    (info ? 'contact' AND jsonb_typeof(info->'contact') = 'object')
    OR (info ? 'username' AND info ? 'password')
);

-- One user per contact value. Rows without a given contact type index as
-- NULL, which never conflicts. Sign-in looks users up with these expressions.
CREATE UNIQUE INDEX users_contact_email_unique ON users ((info->'contact'->>'email'));
CREATE UNIQUE INDEX users_contact_mobile_unique ON users ((info->'contact'->>'mobile'));
CREATE UNIQUE INDEX users_contact_username_unique ON users ((info->'contact'->>'username'));
