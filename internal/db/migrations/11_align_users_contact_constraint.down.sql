-- Migration: 11_align_users_contact_constraint.down.sql
-- Description: Restore the users.info constraint from migration 2.

DROP INDEX IF EXISTS users_contact_username_unique;
DROP INDEX IF EXISTS users_contact_mobile_unique;
DROP INDEX IF EXISTS users_contact_email_unique;

ALTER TABLE users DROP CONSTRAINT IF EXISTS user_info_contact;

-- NOT VALID: users created by OTP sign-in do not satisfy the original rule.
ALTER TABLE users ADD CONSTRAINT user_info_structure CHECK (
    info ? 'username' AND
    info ? 'password'
) NOT VALID;
